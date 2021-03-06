package utils

import (
	"context"
	"errors"
	"fmt"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"log"
	"reflect"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var (
	logger *zap.Logger
	ctx    = context.Background() // 永不超时
)

func init() {
	logger = NewLogger()
}
func NewLogger() *zap.Logger {
	cfg := zap.Config{
		Level:       zap.NewAtomicLevelAt(zap.InfoLevel),
		Development: true,
		Encoding:    "json",
		EncoderConfig: zapcore.EncoderConfig{
			TimeKey:      "time",
			LevelKey:     "level",
			CallerKey:    "caller",
			MessageKey:   "msg",
			LineEnding:   zapcore.DefaultLineEnding,
			EncodeLevel:  zapcore.LowercaseLevelEncoder,
			EncodeTime:   zapcore.ISO8601TimeEncoder, // TimeKey对应的值（时间格式）
			EncodeCaller: zapcore.ShortCallerEncoder, // CallerKey对应的值
		},
		OutputPaths:      []string{"stdout", "./mongosync.log"},
		ErrorOutputPaths: []string{"stderr", "./mongosync.log"},
	}
	logger, err := cfg.Build()
	if err != nil {
		panic(err)
	}
	return logger
}

type NsMap struct {
	SrcDb   string
	SrcColl string
	DstDb   string
	DstColl string
}

type MongoArgs struct {
	ctx                    context.Context
	host                   string
	port                   int
	username               string
	password               string
	authenticationDatabase string
}

type OPLOG struct {
	TS primitive.Timestamp `bson:"ts"`
	T  int64               `bson:"t"`
	H  int64               `bson:"h"`
	V  int                 `bson:"v"`
	OP string              `bson:"op"`
	NS string              `bson:"ns"`
	O2 interface{}         `bson:"o2"`
	O  interface{}         `bson:"o"`
}

// MongoArgs的构造函数
func NewMongoArgs() *MongoArgs {
	return &MongoArgs{
		ctx:                    context.Background(),
		host:                   "0.0.0.0",
		port:                   27017,
		username:               "",
		password:               "",
		authenticationDatabase: "",
	}
}

// 设置上下文
func (mc *MongoArgs) SetContext(ctx context.Context) *MongoArgs {
	mc.ctx = ctx
	return mc
}

// 设置host地址
func (mc *MongoArgs) SetHost(host string) *MongoArgs {
	mc.host = host
	return mc
}

// 设置port
func (mc *MongoArgs) SetPort(port int) *MongoArgs {
	mc.port = port
	return mc
}

// 设置认证用户名
func (mc *MongoArgs) SetUsername(username string) *MongoArgs {
	mc.username = username
	return mc
}

// 设置认证密码
func (mc *MongoArgs) SetPassword(password string) *MongoArgs {
	mc.password = password
	return mc
}

// 设置认证库名称
func (mc *MongoArgs) SetAuthenticationDatabase(authdb string) *MongoArgs {
	mc.authenticationDatabase = authdb
	return mc
}

//创建一个数据库连接，返回一个mongo.Client对象的指针
func (mc *MongoArgs) Connect() *mongo.Client {
	// 设置ctx的默认值
	if mc.ctx == nil {
		mc.ctx = context.Background()
	}
	// 设置port默认值
	if mc.port == 0 {
		mc.port = 27017
	}
	// 设置host默认值
	if mc.host == "" {
		mc.host = "0.0.0.0"
	}
	//认证参数设置，否则连不上
	opts := &options.ClientOptions{}
	opts.ApplyURI(fmt.Sprintf("mongodb://%s:%d", mc.host, mc.port))
	if mc.username != "" && mc.password != "" && mc.authenticationDatabase != "" {
		opts.SetAuth(options.Credential{
			AuthMechanism: "SCRAM-SHA-1",
			AuthSource:    mc.authenticationDatabase,
			Username:      mc.username,
			Password:      mc.password})
	}
	conn, err := mongo.Connect(mc.ctx, opts)
	if err != nil {
		log.Fatal(fmt.Sprintf("mongodb://%s:%d", mc.host, mc.port), "连接MongoDB失败：", err)
	}
	return conn
}

func CustSyncIndex(srcMongo *MongoArgs, srcDbName string, srcCollName string, dstMongo *MongoArgs, dstDbName string, dstCollName string) {
	// 查看索引
	srcClient := srcMongo.Connect()
	defer srcClient.Disconnect(srcMongo.ctx)
	srcColl := srcClient.Database(srcDbName).Collection(srcCollName)
	// ctx:=srcMongo.ctx
	//ctx, _ := context.WithTimeout(context.Background(), 30*time.Second)
	cur, err := srcColl.Indexes().List(ctx) // 查看所有的索引
	if err != nil {
		log.Fatal("查看索引失败：", err)
	}
	defer cur.Close(ctx)
	// 遍历索引，处理索引，插入索引
	for cur.Next(ctx) {
		// TODO: 使用bulk 批量顺序写入，对于批量写入失败的，再使用单条写入
		var indexresult bson.M
		err := cur.Decode(&indexresult)
		if err != nil {
			log.Fatal(err)
		}

		indexopt := options.Index()
		//通过在创建索引时加 background:true 的选项，让创建工作在后台执行。
		//对于存在大量数据的collection中创建索引时使用。我们在插入数据前创建索引，所以无需此项设置
		if value, exists := indexresult["name"]; exists {
			indexopt.SetName(value.(string)) // 索引名称
		}

		if value, exists := indexresult["unique"]; exists {
			indexopt.SetUnique(value.(bool)) // 唯一索引
		}
		if value, exists := indexresult["sparse"]; exists {
			indexopt.SetSparse(value.(bool)) // 稀疏索引
		}
		if value, exists := indexresult["expireAfterSeconds"]; exists {
			indexopt.SetExpireAfterSeconds(value.(int32)) // TTL indexes
		}
		if value, exists := indexresult["partialFilterExpression"]; exists {
			indexopt.SetPartialFilterExpression(value) // 部分索引
		}

		// Changed in version 3.0: The dropDups option is no longer available.
		// 在建立唯一索引时是否删除重复记录,指定 true 创建唯一索引。默认值为 false.
		// if value, exists := indexresult["dropDups"]; exists {
		// 	indexopt.setd(value.(string))
		// }

		// for text index
		// 索引权重值，数值在 1 到 99,999 之间，表示该索引相对于其他索引字段的得分权重。
		if value, exists := indexresult["weights"]; exists {
			indexopt.SetWeights(value)
		}
		// 对于文本索引，该参数决定了停用词及词干和词器的规则的列表。 默认为英语
		if value, exists := indexresult["default_language"]; exists {
			indexopt.SetDefaultLanguage(value.(string))
		}
		// 对于文本索引，该参数指定了包含在文档中的字段名，语言覆盖默认的language，默认值为 language.
		if value, exists := indexresult["language_override"]; exists {
			indexopt.SetLanguageOverride(value.(string))
		}
		indexmodel := mongo.IndexModel{}
		if value, exists := indexresult["key"]; exists {
			indexmodel.Keys = value
			indexmodel.Options = indexopt
		}
		//ctx, _ = context.WithTimeout(context.Background(), 30*time.Second)
		dstClient := dstMongo.Connect()
		defer dstClient.Disconnect(dstMongo.ctx)
		dstColl := dstClient.Database(dstDbName).Collection(dstCollName)
		_, err = dstColl.Indexes().CreateOne(ctx, indexmodel)
		if err != nil {
			log.Fatalf("db[%s].coll[%s]索引[%s]添加失败：%v\n", dstDbName, dstCollName, *(indexopt.Name), err)
		}
	}
}

func CustSyncCollection(srcMongo *MongoArgs, srcDbName string, srcCollName string, dstMongo *MongoArgs, dstDbName string, dstCollName string, updateOverwrite bool, noIndex bool) {
	start := time.Now()
	// TODO: 处理网络断开，自动重连——比如dbserver重启后自动重连

	// 同步索引
	if !noIndex {
		CustSyncIndex(srcMongo, srcDbName, srcCollName, dstMongo, dstDbName, dstCollName)
	}
	// 同步文档
	// 连接src数据库
	srcClient := srcMongo.Connect()
	defer srcClient.Disconnect(srcMongo.ctx)
	srcColl := srcClient.Database(srcDbName).Collection(srcCollName)
	// 连接dst数据库
	dstClient := dstMongo.Connect()
	defer dstClient.Disconnect(dstMongo.ctx)
	dstColl := dstClient.Database(dstDbName).Collection(dstCollName)
	//ctx:=srcMongo.ctx
	//ctx, _ := context.WithTimeout(context.Background(), 30*time.Second)
	//创建findoptions参数
	findOpts := options.Find()
	findOpts.SetCursorType(options.NonTailable)
	findOpts.SetSnapshot(true)
	findOpts.SetNoCursorTimeout(true)
	filter := bson.M{}
	cur, err := srcColl.Find(ctx, filter, findOpts)
	CheckErr(err)
	defer cur.Close(ctx)

	//处理cur，并插入
	var doc interface{}
	var docs []interface{}
	var docNum, insertedNum int64

	for cur.Next(ctx) {
		err := cur.Decode(&doc)
		// cur.Current // bson.Raw数据类型
		// cur.Current.Lookup("key1", "key2") //判断是否含有某个键
		// instock, err := cur.Current.LookupErr()
		// instock.Value()
		// instock.Array().Values()
		if err != nil {
			logger.Fatal(err.Error())
		} else {
			docNum++
			docs = append(docs, doc)
		}
		if docNum%10000 == 0 { // 插入  ,此处可以控制批量插入的条数。可以设置1w/次
			sucessNum, failNum := CustInsertMany(dstColl, docs, updateOverwrite)
			if failNum != 0 {
				logger.Fatal("insert data err！")
			} else {
				insertedNum += sucessNum
				docs = []interface{}{}
			}
		}
	}
	if len(docs) > 0 {
		sucessNum, failNum := CustInsertMany(dstColl, docs, updateOverwrite)
		if failNum != 0 {
			logger.Fatal("insert data err！")
		} else {
			insertedNum += sucessNum
			docs = []interface{}{}
		}
	}
	end := time.Now()
	duration := fmt.Sprintf("%.2f", end.Sub(start).Seconds())
	fmt.Printf("%s数据导入完成，导入数量：%v，耗时：%v秒\n", srcDbName+"."+srcCollName, insertedNum, duration)
}

// 对mongo.Collection对象进行批量插入，如果批量插入失败，则转换为逐条插入
func CustInsertMany(coll *mongo.Collection, docs []interface{}, updateOverwrite bool) (sucessNum int64, failNum int64) {
	// 设置	InsertMany相关参数
	//ctx, _ := context.WithTimeout(context.Background(), 30*time.Second)
	insertManyOpts := options.InsertMany()
	insertManyOpts.SetOrdered(true)                   // true:按docs顺序逐条插入，遇到错误，终止插入；  false：:按docs顺序逐条插入，遇到错误，跳过错误的记录，继续插入后面的记录
	insertManyOpts.SetBypassDocumentValidation(false) //Mongodb提供了在插入和更新时验证文档的功能。就是一种约束条件

	docsNum := int64(len(docs))
	_, err := coll.InsertMany(context.Background(), docs, insertManyOpts) // insertManyResult无论是否插入成功，都会显示docs中所有的_id
	if err != nil {
		var docsChan = make(chan interface{}, 1000)
		var lock sync.Mutex
		// 生产者
		go func(docsChan chan interface{}) {
			for _, doc := range docs {
				docsChan <- doc
			}
			close(docsChan)
		}(docsChan)

		// 业务
		insertManyErrHandler := func(doc interface{}) {
			if updateOverwrite { // 采用replaceOne方式，覆盖已经存在的_id记录
				ReplaceOneOpts := options.Replace()
				ReplaceOneOpts.SetBypassDocumentValidation(false) //Mongodb提供了在插入和更新时验证文档的功能。就是一种约束条件
				ReplaceOneOpts.SetUpsert(true)                    // 如果未查询到，则新建
				filter := bson.M{"_id": doc.(bson.D).Map()["_id"]}
				replaceOne, err := coll.ReplaceOne(ctx, filter, doc, ReplaceOneOpts)
				if err != nil { // ReplaceOne操作失败，failNum加1
					lock.Lock()
					failNum++
					lock.Unlock()
					logger.Error(err.Error(), zap.String("NS", coll.Database().Name()+"."+coll.Name()), zap.String("doc", fmt.Sprintf("%v", doc)))
				} else {
					lock.Lock()
					sucessNum++
					lock.Unlock()
					logger.Debug("ReplaceOne操作成功", zap.String("NS", coll.Database().Name()+"."+coll.Name()), zap.String("UpsertedID", fmt.Sprintf("%v", replaceOne.UpsertedID)), zap.String("doc", fmt.Sprintf("%v", doc)))
				}
			} else { // 采用insertOne方式，忽略_id已经存在的记录，不做任何操作
				insertOneOpts := options.InsertOne()
				insertOneOpts.SetBypassDocumentValidation(true)
				insertOneResult, err := coll.InsertOne(ctx, doc, insertOneOpts)
				if err != nil {
					if strings.Contains(err.Error(), "E11000 duplicate key error") { // 1、违反唯一约束错误，忽略错误
						lock.Lock()
						sucessNum++
						lock.Unlock()
						logger.Debug(err.Error(), zap.String("NS", coll.Database().Name()+"."+coll.Name()), zap.String("doc", fmt.Sprintf("%v", doc)))
					} else { // 2、除唯一约束错误之外的其他错误
						lock.Lock()
						failNum++
						lock.Unlock()
						logger.Error(err.Error(), zap.String("NS", coll.Database().Name()+"."+coll.Name()), zap.String("doc", fmt.Sprintf("%v", doc)))
					}
				} else { // 3、没有错误
					lock.Lock()
					sucessNum++
					lock.Unlock()
					logger.Debug("InsertOne操作成功", zap.String("NS", coll.Database().Name()+"."+coll.Name()), zap.String("UpsertedID", fmt.Sprintf("%v", insertOneResult.InsertedID)), zap.String("doc", fmt.Sprintf("%v", doc)))
				}
			}
		}

		// 消费者：
		worker := func(wg *sync.WaitGroup) {
			for doc := range docsChan { // channel是线程安全的，多个消费者可以同时操作channel
				insertManyErrHandler(doc)
			}
			wg.Done()
		}

		//WorkerPool
		func(numOfWorkers int) {
			var wg sync.WaitGroup
			for i := 0; i < numOfWorkers; i++ {
				wg.Add(1)
				go worker(&wg)
			}
			wg.Wait()
		}(500)
	} else { // InsertMany批量插入成功
		sucessNum = int64(docsNum)
	}
	logger.Info("InsertMany批量插入数据", zap.String("NS", coll.Database().Name()+"."+coll.Name()), zap.Int64("docsNum", docsNum), zap.Int64("sucessNum", sucessNum), zap.Int64("failNum", failNum))
	return sucessNum, failNum
}

// 获取当前最新的oplog对应的timestamp：需要访问admin权限
func CustGetLatestOplogTimestamp(srcMongo *MongoArgs) (primitive.Timestamp, error) {
	// TODO ：是否有访问admin库的权限
	// 从3.2版本开始，oplog中的ts表示发生了变化：。
	// Refer to https://docs.mongodb.com/manual/reference/command/replSetGetStatus/
	srcClient := srcMongo.Connect()
	defer srcClient.Disconnect(srcMongo.ctx)

	var res bson.M
	err := srcClient.Database("admin").RunCommand(context.Background(), bson.D{{"replSetGetStatus", 1}}).Decode(&res)
	if err != nil {
		return primitive.Timestamp{}, err
	}
	for _, member := range res["members"].(bson.A) {
		if member.(bson.M)["stateStr"] == "PRIMARY" {
			if reflect.TypeOf(member.(bson.M)["optime"]).Kind() == reflect.Map { // version≥3.2
				return member.(bson.M)["optime"].(bson.M)["ts"].(primitive.Timestamp), nil
			} else { // version<3.2
				return member.(bson.M)["optime"].(primitive.Timestamp), nil
			}
		}
	}
	return primitive.Timestamp{}, errors.New("no oplog timestamp status")
}

// 对指定的ns进行oplog重放,oplog来自srcMongo对应实例的srcOplogNamespace集合。
// 如果endTS=primitive.Timestamp{}，默认行为为实时重放oplog。即使用tail模式的游标
// srcOplogNamespace表示oplog存放的collection，如果为空字符串，则表示使用默认的"local.oplog.rs"
// nsSlice表示仅对这些ns进行oplog replay；
// nsnsMap 表示对这里面的ns进行名称空间映射；
func CustReplayOplog(srcMongo, dstMongo *MongoArgs, startTS, endTS primitive.Timestamp, srcOplogNamespace string, nsSlice []string, nsnsMap map[string]string) {
	var err error
	//oplog来源集合，srcOplogNsSlice格式为：[local,oplog.rs]
	if srcOplogNamespace == "" {
		srcOplogNamespace = "local.oplog.rs"
	}
	srcOplogNsSlice := strings.SplitN(srcOplogNamespace, ".", 2)
	if len(srcOplogNsSlice) != 2 {
		log.Fatalln("srcOplogNamespace默认oplog名称空间格式有误!")
	}
	// 连接src、dst数据库
	srcClient := srcMongo.Connect()
	defer srcClient.Disconnect(srcMongo.ctx)
	dstClient := dstMongo.Connect()
	defer dstClient.Disconnect(context.Background())

	srcColl := srcClient.Database(srcOplogNsSlice[0]).Collection(srcOplogNsSlice[1])
	// 验证startTS有效性，如果失效，直接退出。
	var firstoplog bson.M
	err = srcColl.FindOne(context.Background(), bson.M{"ts": bson.M{"$gte": startTS}}).Decode(&firstoplog)
	if err != nil {
		log.Fatalln("验证startTS有效性时，查询失败：", err)
	} else if !firstoplog["ts"].(primitive.Timestamp).Equal(startTS) {
		log.Fatalf("由于固定集合%s的size太小或者全量备份时间太长，导致startTS指定的那条oplog记录已经被覆盖，终止oplog重放操作!请使用--sync_oplog参数重新进行同步操作，此时会将oplog记录到目标mongodb中的syncoplog.oplog.rs中，然后使用--replayoplog参数手动重放", srcOplogNamespace)
	}
	// Tailable游标只能用在固定集合上,如果oplog来源自local.oplog.rs，则使用Tailable，否则使用NonTailable
	// 判断endTS是否为空,如果为空，则或者从startTS开始的所有记录
	var filter bson.D
	findOpts := options.Find()
	if srcOplogNamespace == "local.oplog.rs" {
		findOpts.SetCursorType(options.TailableAwait) //Tailable游标只能用在固定集合上
		findOpts.SetNoCursorTimeout(true)
	} else {
		findOpts.SetCursorType(options.NonTailable)
		findOpts.SetNoCursorTimeout(true)
	}
	if endTS.T == 0 && endTS.I == 0 {
		filter = bson.D{{"ts", bson.D{{"$gte", startTS}}}}
	} else {
		filter = bson.D{{"$and", bson.D{{"ts", bson.M{"$gte": startTS}}, {"ts", bson.M{"$lte": endTS}}}}}
	}

	// 判断 nsSlice中是否存在指定的 ns。
	// 如果ns为db.$cmd类型的，只判断db部分，如果db存在指定列表中，则CustContainsNs为true。
	CustContainsNs := func(oplogns string, nsSlice []string) bool {
		// 如果CustReplayOplog指定nsSlice参数为空，则默认对所有ns的oplog进行重放
		// if len(nsSlice) == 0 {
		// 	return true
		// }
		for _, value := range nsSlice {
			if oplogns == value {
				return true
			}
			if strings.HasPrefix(value, strings.TrimSuffix(oplogns, "$cmd")) {
				// 如果指定collection，重放c类型的oplog可能会报错:因为u操作对应的collection可能不存在
				return true
			}
		}
		return false
	}

	// 获取cursor
	cur, err := srcColl.Find(context.Background(), filter, findOpts)
	if err != nil {
		log.Fatal(err)
	}
	defer cur.Close(context.Background())

	var (
		oplog      OPLOG
		oplogBsonD primitive.D
	)
	//var oplog_bsonD bson.D // TODO: bson.D格式的处理
	for cur.Next(context.Background()) {
		// 获取oplog记录
		if err := cur.Err(); err != nil {
			log.Fatal(err)
		}
		err := cur.Decode(&oplog)
		if err != nil {
			log.Fatal(err)
		}
		err = cur.Decode(&oplogBsonD)
		if err != nil {
			log.Fatal(err)
		}
		// 测试当前oplog是不是当前最新的oplog（新产生的oplog）。
		// 只适用于固定集合local.oplog.rs。对于指定endTS的情况（不为空）无需进行判断
		if srcOplogNamespace == "local.oplog.rs" && endTS.T == 0 && endTS.I == 0 {
			currentTS, err := CustGetLatestOplogTimestamp(srcMongo)
			if err != nil {
				log.Println("获取当前最新的oplog对应的timestamp失败：", err)
			} else if currentTS.Equal(oplog.TS) {
				//} else if currentTS.Equal(oplog[0].Value.(primitive.Timestamp)) {
				// 比较oplog中的timestamp和当前最新的timestamp是否相等
				log.Println("正在实时重放当前最新生成的oplog，您可以\"ctrl+c\"手动终止程序!  当前oplog为:", oplogBsonD)
			} else {
			}
		}

		// oplog replay 逐条进行，TODO：使用bulk提高写入效率
		dstDbName, dstCollName := CustGetOplogNs(oplog)
		if CustContainsNs(fmt.Sprintf("%s.%s", dstDbName, dstCollName), nsSlice) { // 仅对指定的ns相关的oplog进行重放
			nsStruct := CustFilter(fmt.Sprintf("%s.%s", dstDbName, dstCollName), nsnsMap) //  对ns进行名称空间映射处理
			dstDb := dstClient.Database(nsStruct.DstDb)
			dstColl := dstDb.Collection(nsStruct.DstColl)
			switch oplog.OP {
			case "i":
				if _, exists := oplog.O.(bson.D).Map()["_id"]; exists {
					ReplaceOneOpts := options.Replace()
					ReplaceOneOpts.SetUpsert(true)
					_, err := dstColl.ReplaceOne(context.Background(), bson.M{"_id": oplog.O.(bson.D).Map()["_id"]}, oplog.O, ReplaceOneOpts)
					if err != nil {
						log.Println("oplog执行'i'操作失败：", err, "\toplog内容：", oplogBsonD)
					}
				} else {
					// 创建索引的oplog
					indexopt := options.Index()
					indexopt.SetName(oplog.O.(bson.D).Map()["name"].(string))
					indexopt.SetBackground(true)

					indexmodel := mongo.IndexModel{}
					indexmodel.Keys = oplog.O.(bson.D).Map()["key"]
					indexmodel.Options = indexopt
					_, err := dstColl.Indexes().CreateOne(context.Background(), indexmodel)
					if err != nil {
						log.Println("oplog创建索引失败：", err, "\toplog内容：", oplogBsonD)
					}
				}
			case "u":
				if _, exists := oplog.O.(bson.D).Map()["$set"]; exists {
					UpdateOpts := options.Update()
					UpdateOpts.SetUpsert(true)
					UpdateOpts.SetBypassDocumentValidation(false)

					_, err := dstColl.UpdateOne(context.Background(), oplog.O2, oplog.O, UpdateOpts) // update操作
					if err != nil {
						log.Println("oplog执行'u'操作失败：", err, "\toplog内容：", oplogBsonD)
					}
				} else {
					ReplaceOneOpts := options.Replace()
					ReplaceOneOpts.SetUpsert(true)
					_, err := dstColl.ReplaceOne(context.Background(), oplog.O2, oplog.O, ReplaceOneOpts) // replace操作
					if err != nil {
						log.Println("oplog执行'u'操作失败：", err, "\toplog内容：", oplogBsonD)
					}
				}
			case "d":
				_, err := dstColl.DeleteOne(context.Background(), oplog.O)
				if err != nil {
					log.Println("oplog执行'd'操作失败：", err, "\toplog内容：", oplogBsonD)
				}
			case "c": // command,集合映射时，可能导致失败
				res := dstDb.RunCommand(context.Background(), oplog.O)
				if err := res.Err(); err != nil {
					log.Println("oplog执行'c'操作失败：", err, "\toplog内容：", oplogBsonD)
				}
			case "n":
				// noop：do nothing
			default:
				log.Println("未识别的oplog操作：", "\toplog内容：", oplogBsonD)
			}
		}
	}
}

//根据oplog获取oplog对应的Namespace。
// noop类型的oplog返回空；command类型的oplog，第二个返回值为:$cmd
func CustGetOplogNs(oplog OPLOG) (string, string) {
	defer func() {
		if err := recover(); err != nil {
			log.Println(err, "\toplog内容：", oplog)
		}
	}()

	if oplog.NS != "" { // 非o="n"的oplog,其ns为空
		var NS []string
		_, exists := oplog.O.(bson.D).Map()["_id"] //如果oplog["o"]中存在"_id"字段，表示普通类型的insert操作；否则为创建索引的操作
		if oplog.OP == "i" && !exists {
			// 针对于创建索引的i类型的oplog。
			NS = strings.SplitN(oplog.O.(bson.D).Map()["ns"].(string), ".", 2)
			// 	例如：
			// 	{
			// 		"ts" : Timestamp(1553916471, 1),
			// 		"t" : NumberLong(7),
			// 		"h" : NumberLong("-2657638637154273180"),
			// 		"v" : 2,
			// 		"op" : "i",
			// 		"ns" : "GlobalDB.system.indexes",
			// 		"o" : {
			// 				"v" : 2,
			// 				"key" : {
			// 						"servicename" : 1
			// 				},
			// 				"name" : "servicename_1",
			// 				"ns" : "GlobalDB.GlobalService"
			// 		}
			// }
		} else {
			// 针对于普通类i类型及其他各种类型的oplog。
			NS = strings.SplitN(oplog.NS, ".", 2)
			// 	例如：普通的i类型的oplog
			// 	{
			// 		"ts" : Timestamp(1547796424, 1),
			// 		"t" : NumberLong(1),
			// 		"h" : NumberLong("5493022917460612893"),
			// 		"v" : 2,
			// 		"op" : "i",
			// 		"ns" : "CUST_U_TEST.GlobalService",
			// 		"o" : {
			// 				"_id" : ObjectId("5c417fc8164a04e1f36777f1"),
			// 				"id" : "baa00003",
			// 				"servicename" : "",
			// 				"serviceindex" : "Tp.Sys.CustomField",
			// 				"serviceobject" : "CustomFieldSetupService.execute",
			// 				"servicepath" : "com.g3cloud.platform.ui.setup.service.customfields",
			// 				"servicetype" : ""
			// 		}
			// }
			// 	例如： u类型的oplog(update操作)
			// 	{
			// 		"ts" : Timestamp(1553916741, 1),
			// 		"t" : NumberLong(7),
			// 		"h" : NumberLong("-7145600835364117702"),
			// 		"v" : 2,
			// 		"op" : "u",
			// 		"ns" : "GlobalDB.GlobalService",
			// 		"o2" : {
			// 				"_id" : ObjectId("5b0e13b0fa23fd45bd125dfb")
			// 		},
			// 		"o" : {
			// 				"$set" : {
			// 						"servicename" : "测试444"
			// 				}
			// 		}
			// }
			// 	例如： u类型的oplog(replace操作)
			//{
			//		"ts" : Timestamp(1561014006, 1),
			//		"t" : NumberLong(1),
			//		"h" : NumberLong("-3250511269367634318"),
			//		"v" : 2,
			//		"op" : "u",
			//		"ns" : "GlobalDB.GlobalService",
			//		"o2" : { "_id" : ObjectId("5d09c820612622a9758da8ee") },
			//		"o" : {
			//				"_id" : ObjectId("5d09c820612622a9758da8ee"),
			//				"id" : "baa00001", "servicename" : "PPPPPPPPPPPPPPPPPPP",
			//				"serviceindex" : "Tp.Sys.CustomObject.Query",
			//				"serviceobject" : "CustomObjectSetupService.query",
			//				"servicepath" : "com.g3cloud.platform.ui.setup.service.customobjects",
			//				"servicetype" : "1112"
			//		}
			//}
			// 	例如： c类型的oplog，较特殊，coll为 "$cmd"
			// 	{
			// 		"ts" : Timestamp(1553916897, 1),
			// 		"t" : NumberLong(7),
			// 		"h" : NumberLong("7885702576673444906"),
			// 		"v" : 2,
			// 		"op" : "c",
			// 		"ns" : "GlobalDB.$cmd",
			// 		"o" : {
			// 				"deleteIndexes" : "GlobalService",
			// 				"index" : "servicename_1"
			// 		}
			// }
			// 	例如： d类型的oplog
			// 	{
			// 		"ts" : Timestamp(1553916996, 1),
			// 		"t" : NumberLong(7),
			// 		"h" : NumberLong("-8661158816231912995"),
			// 		"v" : 2,
			// 		"op" : "d",
			// 		"ns" : "GlobalDB.GlobalService",
			// 		"o" : {
			// 				"_id" : ObjectId("5b0e13b0fa23fd45bd125dfb")
			// 		}
			// }
		}
		return NS[0], NS[1]
	}
	// 针对于n类型的oplog。
	return "", ""
	// 	例如：
	// 	{
	//         "ts" : Timestamp(1553916453, 1),
	//         "t" : NumberLong(7),
	//         "h" : NumberLong("3294559918570847780"),
	//         "v" : 2,
	//         "op" : "n",
	//         "ns" : "",
	//         "o" : {
	//                 "msg" : "periodic noop"
	//         }
	// }
}

// 从src库同步oplog到dst的库中，用于手动重放
func CustSyncOplog(srcMongo *MongoArgs, dstMongo *MongoArgs, startTS primitive.Timestamp) {
	// TODO: 处理网络断开，自动重连——比如dbserver重启后自动重连
	// TODO:  判断如果syncoplog库存在数据，退出

	const (
		srcDbName   string = "local"
		srcCollName string = "oplog.rs"
		dstDbName   string = "syncoplog"
		dstCollName string = "oplog.rs"
	)
	srcClient := srcMongo.Connect()
	defer srcClient.Disconnect(srcMongo.ctx)
	dstClient := dstMongo.Connect()
	defer dstClient.Disconnect(dstMongo.ctx)

	srcColl := srcClient.Database(srcDbName).Collection(srcCollName)
	//创建findoptions参数
	findOpts := options.Find()
	findOpts.SetCursorType(options.TailableAwait)
	findOpts.SetNoCursorTimeout(true)
	filter := bson.D{{"ts", bson.D{{"$gte", startTS}}}}

	// 验证startTS有效性，如果失效，直接退出。
	var firstoplog bson.M
	time.Sleep(5e9)
	err := srcColl.FindOne(context.Background(), filter).Decode(&firstoplog)
	if err != nil {
		log.Fatalln("验证startTS有效性时，查询失败：", err)
	} else if !firstoplog["ts"].(primitive.Timestamp).Equal(startTS) {
		log.Fatalln("startTS指定的oplog已经失效，终止syncoplog操作")
	}

	cur, err := srcColl.Find(context.Background(), filter, findOpts)
	if err != nil {
		log.Fatal(err)
	}
	defer cur.Close(context.Background())

	var oplog bson.M
	for cur.Next(context.Background()) {
		if err := cur.Err(); err != nil {
			log.Fatal(err)
		}
		err := cur.Decode(&oplog)
		if err != nil {
			log.Fatal("Decode oplog into variable err:", err)
		}

		currentTS, err := CustGetLatestOplogTimestamp(srcMongo)
		if err != nil {
			log.Println("获取当前最新的oplog对应的timestamp失败：", err)
		} else if currentTS.Equal(oplog["ts"].(primitive.Timestamp)) {
			// 比较oplog中的timestamp和当前最新的timestamp是否相等
			log.Printf("正在实时同步最新生成的oplog到%s.%s，您可以'ctrl+c'手动终止程序!当前同步的oplog为%s:", dstDbName, dstCollName, oplog)
		}

		dstColl := dstClient.Database(dstDbName).Collection(dstCollName)
		insertOneOpts := options.InsertOne()
		insertOneOpts.SetBypassDocumentValidation(false)
		_, err = dstColl.InsertOne(context.Background(), oplog, insertOneOpts)
		if err != nil {
			log.Fatalln("syncoplog插入oplog失败：", err)
		}
	}
}

// 获取指定mongodb实例的数据库列表,排查admin和local库
func CustGetDbs(src *MongoArgs) []string {
	dbs, err := src.Connect().ListDatabaseNames(context.Background(), bson.M{})
	if err != nil {
		log.Fatalln("获取mongodb实例中的数据库列表失败：", err)
	}
	i := 0
	for _, db := range dbs {
		if db != "local" && db != "admin" {
			dbs[i] = db
			i++
		}
	}
	// 以下操作内存优化的考虑
	tmp := dbs[:i]
	newdbs := make([]string, len(tmp))
	copy(newdbs, tmp)
	return newdbs
}

// 获取指定数据库中的集合列表
func CustGetColls(src *MongoArgs, dbName string) []string {
	srcClient := src.Connect()
	defer srcClient.Disconnect(context.Background())
	cur, err := srcClient.Database(dbName).ListCollections(context.Background(), bson.M{})
	if err != nil {
		log.Fatalln("获取指定数据库中的集合列表失败：", err)
	}
	defer cur.Close(context.Background())
	var doc bson.M
	var collnames []string
	for cur.Next(context.Background()) {
		err := cur.Decode(&doc)
		CheckErr(err)
		collnames = append(collnames, doc["name"].(string))
	}
	return collnames
}

//删除切片中第一个给定的元素
func CustStringSliceRemove(slice []string, element string) []string {
	for index, value := range slice {
		if value == element {
			slice = append(slice[:index], slice[index+1:]...)
			break
		}
	}
	return slice
}

// 判断切片中是否含有给定元素
func CustStringSliceHas(slice []string, element string) bool {
	for _, value := range slice {
		if element == value {
			return true
		}
	}
	return false
}

//NsMap是一个key为srcNs，value为dstNs的字典。传入一个ns，返回一个*NsMap结构体
func CustFilter(ns string, nsnsMap map[string]string) *NsMap {
	if _, exist := nsnsMap[ns]; exist {
		return &NsMap{
			SrcDb:   strings.SplitN(ns, ".", 2)[0],
			SrcColl: strings.SplitN(ns, ".", 2)[1],
			DstDb:   strings.SplitN(nsnsMap[ns], ".", 2)[0],
			DstColl: strings.SplitN(nsnsMap[ns], ".", 2)[1],
		}
	} else {
		return &NsMap{
			SrcDb:   strings.SplitN(ns, ".", 2)[0],
			SrcColl: strings.SplitN(ns, ".", 2)[1],
			DstDb:   strings.SplitN(ns, ".", 2)[0],
			DstColl: strings.SplitN(ns, ".", 2)[1],
		}
	}
}

func CheckErr(err error) {
	if err != nil {
		logger.Error(err.Error())
	}
}
