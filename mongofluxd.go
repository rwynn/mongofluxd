package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"github.com/BurntSushi/toml"
	"github.com/globalsign/mgo"
	"github.com/globalsign/mgo/bson"
	"github.com/influxdata/influxdb/client/v2"
	"github.com/rwynn/gtm"
	"github.com/rwynn/mongofluxd/mongofluxdplug"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/signal"
	"plugin"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

var infoLog *log.Logger = log.New(os.Stdout, "INFO ", log.Flags())

const (
	Name                  = "mongofluxd"
	Version               = "0.5.1"
	mongoUrlDefault       = "localhost"
	influxUrlDefault      = "http://localhost:8086"
	influxClientsDefault  = 10
	influxBufferDefault   = 1000
	resumeNameDefault     = "default"
	gtmChannelSizeDefault = 512
)

type mongoDialSettings struct {
	Timeout int
	Ssl     bool
}

type mongoSessionSettings struct {
	SocketTimeout int `toml:"socket-timeout"`
	SyncTimeout   int `toml:"sync-timeout"`
}

type gtmSettings struct {
	ChannelSize    int    `toml:"channel-size"`
	BufferSize     int    `toml:"buffer-size"`
	BufferDuration string `toml:"buffer-duration"`
}

type measureSettings struct {
	Namespace string
	Timefield string
	Retention string
	Precision string
	Measure   string
	Symbol    string
	Tags      []string
	Fields    []string
	plug      func(*mongofluxdplug.MongoDocument) ([]*mongofluxdplug.InfluxPoint, error)
}

type configOptions struct {
	MongoUrl                 string               `toml:"mongo-url"`
	MongoPemFile             string               `toml:"mongo-pem-file"`
	MongoSkipVerify          bool                 `toml:"mongo-skip-verify"`
	MongoOpLogDatabaseName   string               `toml:"mongo-oplog-database-name"`
	MongoOpLogCollectionName string               `toml:"mongo-oplog-collection-name"`
	MongoDialSettings        mongoDialSettings    `toml:"mongo-dial-settings"`
	MongoSessionSettings     mongoSessionSettings `toml:"mongo-session-settings"`
	GtmSettings              gtmSettings          `toml:"gtm-settings"`
	ResumeName               string               `toml:"resume-name"`
	Version                  bool
	Verbose                  bool
	Resume                   bool
	ResumeWriteUnsafe        bool  `toml:"resume-write-unsafe"`
	ResumeFromTimestamp      int64 `toml:"resume-from-timestamp"`
	Replay                   bool
	ConfigFile               string
	Measurement              []*measureSettings
	InfluxUrl                string `toml:"influx-url"`
	InfluxUser               string `toml:"influx-user"`
	InfluxPassword           string `toml:"influx-password"`
	InfluxSkipVerify         bool   `toml:"influx-skip-verify"`
	InfluxPemFile            string `toml:"influx-pem-file"`
	InfluxAutoCreateDB       bool   `toml:"influx-auto-create-db"`
	InfluxClients            int    `toml:"influx-clients"`
	InfluxBufferSize         int    `toml:"influx-buffer-size"`
	DirectReads              bool   `toml:"direct-reads"`
	ExitAfterDirectReads     bool   `toml:"exit-after-direct-reads"`
	PluginPath               string `toml:"plugin-path"`
}

type InfluxMeasure struct {
	ns        string
	timefield string
	retention string
	precision string
	measure   string
	tags      map[string]bool
	fields    map[string]bool
	plug      func(*mongofluxdplug.MongoDocument) ([]*mongofluxdplug.InfluxPoint, error)
}

type InfluxCtx struct {
	m        map[string]client.BatchPoints
	c        client.Client
	dbs      map[string]bool
	measures map[string]*InfluxMeasure
	config   *configOptions
	lastTs   bson.MongoTimestamp
	mongo    *mgo.Session
}

type InfluxDataMap struct {
	op        *gtm.Op
	tags      map[string]string
	fields    map[string]interface{}
	timefield bool
	measure   *InfluxMeasure
	t         time.Time
	name      string
}

func TimestampTime(ts bson.MongoTimestamp) time.Time {
	return time.Unix(int64(ts>>32), 0).UTC()
}

func (ctx *InfluxCtx) saveTs() (err error) {
	if ctx.config.Resume && ctx.lastTs != 0 {
		err = SaveTimestamp(ctx.mongo, ctx.lastTs, ctx.config.ResumeName)
		ctx.lastTs = bson.MongoTimestamp(0)
	}
	return
}

func (ctx *InfluxCtx) setupMeasurements() error {
	mss := ctx.config.Measurement
	if len(mss) > 0 {
		for _, ms := range mss {
			im := &InfluxMeasure{
				ns:        ms.Namespace,
				timefield: ms.Timefield,
				retention: ms.Retention,
				precision: ms.Precision,
				measure:   ms.Measure,
				plug:      ms.plug,
				tags:      make(map[string]bool),
				fields:    make(map[string]bool),
			}
			if im.precision == "" {
				im.precision = "s"
			}
			for _, tag := range ms.Tags {
				im.tags[tag] = true
			}
			for _, field := range ms.Fields {
				im.fields[field] = true
			}
			if im.plug == nil {
				if len(im.fields) == 0 {
					return fmt.Errorf("at least one field is required per measurement")
				}
			}
			ctx.measures[ms.Namespace] = im
		}
		return nil
	} else {
		return fmt.Errorf("at least one measurement is required")
	}
}

func (ctx *InfluxCtx) createDatabase(db string) error {
	if ctx.config.InfluxAutoCreateDB {
		if ctx.dbs[db] == false {
			q := client.NewQuery(fmt.Sprintf(`CREATE DATABASE "%s"`, db), "", "")
			if response, err := ctx.c.Query(q); err != nil || response.Error() != nil {
				if err != nil {
					return err
				} else {
					return response.Error()
				}
			} else {
				ctx.dbs[db] = true
			}
		}
	}
	return nil
}

func (ctx *InfluxCtx) setupDatabase(op *gtm.Op) error {
	db, ns := op.GetDatabase(), op.Namespace
	if _, found := ctx.m[ns]; found == false {
		bp, err := client.NewBatchPoints(client.BatchPointsConfig{
			Database:        db,
			RetentionPolicy: ctx.measures[ns].retention,
			Precision:       ctx.measures[ns].precision,
		})
		if err != nil {
			return err
		}
		ctx.m[ns] = bp
		if err := ctx.createDatabase(db); err != nil {
			return err
		}
	}
	return nil
}

func (ctx *InfluxCtx) writeBatch() (err error) {
	points := 0
	for _, bp := range ctx.m {
		points += len(bp.Points())
		if err = ctx.c.Write(bp); err != nil {
			break
		}
	}
	if ctx.config.Verbose {
		if points > 0 {
			infoLog.Printf("%d points flushed\n", points)
		}
	}
	ctx.m = make(map[string]client.BatchPoints)
	if err == nil {
		err = ctx.saveTs()
	}
	return
}

func (m *InfluxDataMap) istagtype(v interface{}) bool {
	switch v.(type) {
	case string:
		return true
	default:
		return false
	}
}

func (m *InfluxDataMap) isfieldtype(v interface{}) bool {
	switch v.(type) {
	case string:
		return true
	case int:
		return true
	case int32:
		return true
	case int64:
		return true
	case float32:
		return true
	case float64:
		return true
	case bool:
		return true
	default:
		return false
	}
}

func (m *InfluxDataMap) flatmap(prefix string, e map[string]interface{}) map[string]interface{} {
	o := make(map[string]interface{})
	for k, v := range e {
		switch child := v.(type) {
		case map[string]interface{}:
			nm := m.flatmap("", child)
			for nk, nv := range nm {
				o[prefix+k+"."+nk] = nv
			}
		case gtm.OpLogEntry:
			nm := m.flatmap("", child)
			for nk, nv := range nm {
				o[prefix+k+"."+nk] = nv
			}
		default:
			if m.isfieldtype(v) {
				o[prefix+k] = v
			}
		}
	}
	return o
}

func (m *InfluxDataMap) unsupportedType(op *gtm.Op, k string, v interface{}, kind string) {
	log.Printf("Unsupported type %T for %s %s in namespace %s\n", v, kind, k, op.Namespace)
}

func (m *InfluxDataMap) loadName() {
	if m.measure.measure != "" {
		m.name = m.measure.measure
	} else {
		m.name = m.op.GetCollection()
	}
}

func (m *InfluxDataMap) loadKV(k string, v interface{}) {
	if m.measure.tags[k] {
		if m.istagtype(v) {
			m.tags[k] = v.(string)
		} else {
			m.unsupportedType(m.op, k, v, "tag")
		}
	} else if m.measure.fields[k] {
		if m.isfieldtype(v) {
			m.fields[k] = v
		} else {
			m.unsupportedType(m.op, k, v, "field")
		}
	}
}

func (m *InfluxDataMap) loadData() error {
	m.tags = make(map[string]string)
	m.fields = make(map[string]interface{})
	if m.measure.timefield == "" {
		m.t = TimestampTime(m.op.Timestamp)
		m.timefield = true
	}
	for k, v := range m.op.Data {
		if k == "_id" {
			continue
		}
		switch vt := v.(type) {
		case time.Time:
			if m.measure.timefield == k {
				m.t = vt.UTC()
				m.timefield = true
			}
		case bson.MongoTimestamp:
			if m.measure.timefield == k {
				m.t = TimestampTime(vt)
				m.timefield = true
			}
		case map[string]interface{}:
			flat := m.flatmap(k+".", vt)
			for fk, fv := range flat {
				m.loadKV(fk, fv)
			}
		case gtm.OpLogEntry:
			flat := m.flatmap(k+".", vt)
			for fk, fv := range flat {
				m.loadKV(fk, fv)
			}
		default:
			m.loadKV(k, v)
		}
	}
	if m.timefield == false {
		if tf, ok := m.op.Data[m.measure.timefield]; ok {
			return fmt.Errorf("time field %s had type %T, but expected %T", m.measure.timefield, tf, m.t)
		} else {
			return fmt.Errorf("time field %s not found in document", m.measure.timefield)
		}
	} else {
		return nil
	}

}

func (ctx *InfluxCtx) addPoint(op *gtm.Op) error {
	measure := ctx.measures[op.Namespace]
	if measure != nil {
		if err := ctx.setupDatabase(op); err != nil {
			return err
		}
		bp := ctx.m[op.Namespace]
		mapper := &InfluxDataMap{
			op:      op,
			measure: measure,
		}
		mapper.loadName()
		if measure.plug != nil {
			points, err := measure.plug(&mongofluxdplug.MongoDocument{
				Data:       op.Data,
				Namespace:  op.Namespace,
				Database:   op.GetDatabase(),
				Collection: op.GetCollection(),
				Operation:  op.Operation,
			})
			if err != nil {
				return err
			}
			for _, pt := range points {
				pt, err := client.NewPoint(mapper.name, pt.Tags, pt.Fields, pt.Timestamp)
				if err != nil {
					return err
				}
				bp.AddPoint(pt)
			}
		} else {
			if err := mapper.loadData(); err != nil {
				return err
			}
			pt, err := client.NewPoint(mapper.name, mapper.tags, mapper.fields, mapper.t)
			if err != nil {
				return err
			}
			bp.AddPoint(pt)
		}
		ctx.lastTs = op.Timestamp
		if len(bp.Points()) >= ctx.config.InfluxBufferSize {
			if err := ctx.writeBatch(); err != nil {
				return err
			}
		}
	}
	return nil
}

func IsInsertOrUpdate(op *gtm.Op) bool {
	return op.IsInsert() || op.IsUpdate()
}

func NotMongoFlux(op *gtm.Op) bool {
	return op.GetDatabase() != Name
}

func ResumeWork(ctx *gtm.OpCtx, session *mgo.Session, config *configOptions) {
	col := session.DB(Name).C("resume")
	doc := make(map[string]interface{})
	col.FindId(config.ResumeName).One(doc)
	if doc["ts"] != nil {
		ts := doc["ts"].(bson.MongoTimestamp)
		ctx.Since(ts)
	}
	ctx.Resume()
}

func SaveTimestamp(session *mgo.Session, ts bson.MongoTimestamp, resumeName string) error {
	col := session.DB(Name).C("resume")
	doc := make(map[string]interface{})
	doc["ts"] = ts
	_, err := col.UpsertId(resumeName, bson.M{"$set": doc})
	return err
}

func (config *configOptions) onlyMeasured() gtm.OpFilter {
	measured := make(map[string]bool)
	for _, m := range config.Measurement {
		measured[m.Namespace] = true
	}
	return func(op *gtm.Op) bool {
		return measured[op.Namespace]
	}
}

func (config *configOptions) ParseCommandLineFlags() *configOptions {
	flag.StringVar(&config.InfluxUrl, "influx-url", "", "InfluxDB connection URL")
	flag.StringVar(&config.InfluxUser, "influx-user", "", "InfluxDB user name")
	flag.StringVar(&config.InfluxPassword, "influx-password", "", "InfluxDB user password")
	flag.BoolVar(&config.InfluxSkipVerify, "influx-skip-verify", false, "Set true to skip https certificate validation for InfluxDB")
	flag.BoolVar(&config.InfluxAutoCreateDB, "influx-auto-create-db", true, "Set false to disable automatic database creation on InfluxDB")
	flag.StringVar(&config.InfluxPemFile, "influx-pem-file", "", "Path to a PEM file for secure connections to InfluxDB")
	flag.IntVar(&config.InfluxClients, "influx-clients", 0, "The number of concurrent InfluxDB clients")
	flag.IntVar(&config.InfluxBufferSize, "influx-buffer-size", 0, "After this number of points the batch is flushed to InfluxDB")
	flag.StringVar(&config.MongoUrl, "mongo-url", "", "MongoDB connection URL")
	flag.StringVar(&config.MongoPemFile, "mongo-pem-file", "", "Path to a PEM file for secure connections to MongoDB")
	flag.BoolVar(&config.MongoSkipVerify, "mongo-skip-verify", false, "Set to true to skip https certificate validator for MongoDB")
	flag.StringVar(&config.MongoOpLogDatabaseName, "mongo-oplog-database-name", "", "Override the database name which contains the mongodb oplog")
	flag.StringVar(&config.MongoOpLogCollectionName, "mongo-oplog-collection-name", "", "Override the collection name which contains the mongodb oplog")
	flag.StringVar(&config.ConfigFile, "f", "", "Location of configuration file")
	flag.BoolVar(&config.Version, "v", false, "True to print the version number")
	flag.BoolVar(&config.Verbose, "verbose", false, "True to output verbose messages")
	flag.BoolVar(&config.Resume, "resume", false, "True to capture the last timestamp of this run and resume on a subsequent run")
	flag.Int64Var(&config.ResumeFromTimestamp, "resume-from-timestamp", 0, "Timestamp to resume syncing from")
	flag.BoolVar(&config.ResumeWriteUnsafe, "resume-write-unsafe", false, "True to speedup writes of the last timestamp synched for resuming at the cost of error checking")
	flag.BoolVar(&config.Replay, "replay", false, "True to replay all events from the oplog and index them in elasticsearch")
	flag.StringVar(&config.ResumeName, "resume-name", "", "Name under which to load/store the resume state. Defaults to 'default'")
	flag.StringVar(&config.PluginPath, "plugin-path", "", "The file path to a .so file plugin")
	flag.BoolVar(&config.DirectReads, "direct-reads", false, "Set to true to read directly from MongoDB collections")
	flag.BoolVar(&config.ExitAfterDirectReads, "exit-after-direct-reads", false, "Set to true to exit after direct reads are complete")
	flag.Parse()
	return config
}

func (config *configOptions) LoadPlugin() *configOptions {
	if config.PluginPath == "" {
		if config.Verbose {
			infoLog.Println("no plugins detected")
		}
		return config
	}
	p, err := plugin.Open(config.PluginPath)
	if err != nil {
		log.Panicf("Unable to load plugin <%s>: %s", config.PluginPath, err)
	}
	for _, m := range config.Measurement {
		if m.Symbol != "" {
			f, err := p.Lookup(m.Symbol)
			if err != nil {
				log.Panicf("Unable to lookup symbol <%s> for plugin <%s>: %s", m.Symbol, config.PluginPath, err)
			}
			switch f.(type) {
			case func(*mongofluxdplug.MongoDocument) ([]*mongofluxdplug.InfluxPoint, error):
				m.plug = f.(func(*mongofluxdplug.MongoDocument) ([]*mongofluxdplug.InfluxPoint, error))
			default:
				log.Panicf("Plugin symbol <%s> must be typed %T", m.Symbol, m.plug)
			}
		}
	}
	if config.Verbose {
		infoLog.Printf("plugin <%s> loaded succesfully\n", config.PluginPath)
	}
	return config
}

func (config *configOptions) LoadConfigFile() *configOptions {
	if config.ConfigFile != "" {
		var tomlConfig configOptions = configOptions{
			MongoDialSettings:    mongoDialSettings{Timeout: -1},
			MongoSessionSettings: mongoSessionSettings{SocketTimeout: -1, SyncTimeout: -1},
			GtmSettings:          GtmDefaultSettings(),
			InfluxAutoCreateDB:   true,
		}
		if _, err := toml.DecodeFile(config.ConfigFile, &tomlConfig); err != nil {
			panic(err)
		}
		if config.InfluxUrl == "" {
			config.InfluxUrl = tomlConfig.InfluxUrl
		}
		if config.InfluxClients == 0 {
			config.InfluxClients = tomlConfig.InfluxClients
		}
		if config.InfluxBufferSize == 0 {
			config.InfluxBufferSize = tomlConfig.InfluxBufferSize
		}
		if config.InfluxUser == "" {
			config.InfluxUser = tomlConfig.InfluxUser
		}
		if config.InfluxPassword == "" {
			config.InfluxPassword = tomlConfig.InfluxPassword
		}
		if config.InfluxSkipVerify == false {
			config.InfluxSkipVerify = tomlConfig.InfluxSkipVerify
		}
		if config.InfluxAutoCreateDB == true {
			if tomlConfig.InfluxAutoCreateDB == false {
				config.InfluxAutoCreateDB = false
			}
		}
		if config.InfluxPemFile == "" {
			config.InfluxPemFile = tomlConfig.InfluxPemFile
		}
		if config.MongoUrl == "" {
			config.MongoUrl = tomlConfig.MongoUrl
		}
		if config.MongoPemFile == "" {
			config.MongoPemFile = tomlConfig.MongoPemFile
		}
		if config.MongoSkipVerify == false {
			config.MongoSkipVerify = tomlConfig.MongoSkipVerify
		}
		if config.MongoOpLogDatabaseName == "" {
			config.MongoOpLogDatabaseName = tomlConfig.MongoOpLogDatabaseName
		}
		if config.MongoOpLogCollectionName == "" {
			config.MongoOpLogCollectionName = tomlConfig.MongoOpLogCollectionName
		}
		if !config.Verbose && tomlConfig.Verbose {
			config.Verbose = true
		}
		if !config.Replay && tomlConfig.Replay {
			config.Replay = true
		}
		if !config.DirectReads && tomlConfig.DirectReads {
			config.DirectReads = true
		}
		if !config.ExitAfterDirectReads && tomlConfig.ExitAfterDirectReads {
			config.ExitAfterDirectReads = true
		}
		if !config.Resume && tomlConfig.Resume {
			config.Resume = true
		}
		if !config.ResumeWriteUnsafe && tomlConfig.ResumeWriteUnsafe {
			config.ResumeWriteUnsafe = true
		}
		if config.ResumeFromTimestamp == 0 {
			config.ResumeFromTimestamp = tomlConfig.ResumeFromTimestamp
		}
		if config.Resume && config.ResumeName == "" {
			config.ResumeName = tomlConfig.ResumeName
		}
		if config.PluginPath == "" {
			config.PluginPath = tomlConfig.PluginPath
		}
		config.MongoDialSettings = tomlConfig.MongoDialSettings
		config.MongoSessionSettings = tomlConfig.MongoSessionSettings
		config.GtmSettings = tomlConfig.GtmSettings
		config.Measurement = tomlConfig.Measurement
	}
	return config
}

func (config *configOptions) InfluxTLS() (*tls.Config, error) {
	certs := x509.NewCertPool()
	if ca, err := ioutil.ReadFile(config.InfluxPemFile); err == nil {
		certs.AppendCertsFromPEM(ca)
	} else {
		return nil, err

	}
	tlsConfig := &tls.Config{RootCAs: certs}
	return tlsConfig, nil
}

func (config *configOptions) SetDefaults() *configOptions {
	if config.InfluxUrl == "" {
		config.InfluxUrl = influxUrlDefault
	}
	if config.InfluxClients == 0 {
		config.InfluxClients = influxClientsDefault
	}
	if config.InfluxBufferSize == 0 {
		config.InfluxBufferSize = influxBufferDefault
	}
	if config.MongoUrl == "" {
		config.MongoUrl = mongoUrlDefault
	}
	if config.ResumeName == "" {
		config.ResumeName = resumeNameDefault
	}
	if config.MongoUrl != "" {
		// if ssl=true is set on the connection string, remove the option
		// from the connection string and enable TLS because the mgo
		// driver does not support the option in the connection string
		const queryDelim string = "?"
		host_query := strings.SplitN(config.MongoUrl, queryDelim, 2)
		if len(host_query) == 2 {
			host, query := host_query[0], host_query[1]
			r := regexp.MustCompile(`ssl=true&?|&ssl=true$`)
			qstr := r.ReplaceAllString(query, "")
			if qstr != query {
				// ssl detected
				config.MongoDialSettings.Ssl = true
				if qstr == "" {
					config.MongoUrl = host
				} else {
					config.MongoUrl = strings.Join([]string{host, qstr}, queryDelim)
				}
			}
		}
	}
	return config
}

func (config *configOptions) DialMongo() (*mgo.Session, error) {
	ssl := config.MongoDialSettings.Ssl || config.MongoPemFile != ""
	if ssl {
		tlsConfig := &tls.Config{}
		if config.MongoPemFile != "" {
			certs := x509.NewCertPool()
			if ca, err := ioutil.ReadFile(config.MongoPemFile); err == nil {
				certs.AppendCertsFromPEM(ca)
			} else {
				return nil, err
			}
			tlsConfig.RootCAs = certs
		}
		// Check to see if we don't need to validate the PEM
		if config.MongoSkipVerify {
			// Turn off validation
			tlsConfig.InsecureSkipVerify = true
		}
		dialInfo, err := mgo.ParseURL(config.MongoUrl)
		if err != nil {
			return nil, err
		} else {
			dialInfo.Timeout = time.Duration(10) * time.Second
			if config.MongoDialSettings.Timeout != -1 {
				dialInfo.Timeout = time.Duration(config.MongoDialSettings.Timeout) * time.Second
			}
			dialInfo.DialServer = func(addr *mgo.ServerAddr) (net.Conn, error) {
				conn, err := tls.Dial("tcp", addr.String(), tlsConfig)
				if err != nil {
					log.Printf("Unable to dial mongodb: %s\n", err)
				}
				return conn, err
			}
			session, err := mgo.DialWithInfo(dialInfo)
			if err == nil {
				session.SetSyncTimeout(1 * time.Minute)
				session.SetSocketTimeout(1 * time.Minute)
			}
			return session, err
		}
	} else {
		if config.MongoDialSettings.Timeout != -1 {
			return mgo.DialWithTimeout(config.MongoUrl,
				time.Duration(config.MongoDialSettings.Timeout)*time.Second)
		} else {
			return mgo.Dial(config.MongoUrl)
		}
	}
}

func GtmDefaultSettings() gtmSettings {
	return gtmSettings{
		ChannelSize:    gtmChannelSizeDefault,
		BufferSize:     32,
		BufferDuration: "750ms",
	}
}

func main() {
	log.SetPrefix("ERROR ")

	config := &configOptions{
		MongoDialSettings:    mongoDialSettings{Timeout: -1},
		MongoSessionSettings: mongoSessionSettings{SocketTimeout: -1, SyncTimeout: -1},
		GtmSettings:          GtmDefaultSettings(),
	}
	config.ParseCommandLineFlags()
	if config.Version {
		fmt.Println(Version)
		os.Exit(0)
	}
	config.LoadConfigFile().SetDefaults().LoadPlugin()

	sigs := make(chan os.Signal, 1)
	stopC := make(chan bool, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)

	mongo, err := config.DialMongo()
	if err != nil {
		log.Panicf("Unable to connect to mongodb using URL %s: %s", config.MongoUrl, err)
	}
	mongo.SetMode(mgo.Primary, true)
	if config.Resume && config.ResumeWriteUnsafe {
		mongo.SetSafe(nil)
	}
	if config.MongoSessionSettings.SocketTimeout != -1 {
		timeOut := time.Duration(config.MongoSessionSettings.SocketTimeout) * time.Second
		mongo.SetSocketTimeout(timeOut)
	}
	if config.MongoSessionSettings.SyncTimeout != -1 {
		timeOut := time.Duration(config.MongoSessionSettings.SyncTimeout) * time.Second
		mongo.SetSyncTimeout(timeOut)
	}

	go func() {
		<-sigs
		stopC <- true
	}()

	var after gtm.TimestampGenerator = nil
	if config.Resume {
		after = func(session *mgo.Session, options *gtm.Options) bson.MongoTimestamp {
			ts := gtm.LastOpTimestamp(session, options)
			if config.Replay {
				ts = bson.MongoTimestamp(0)
			} else if config.ResumeFromTimestamp != 0 {
				ts = bson.MongoTimestamp(config.ResumeFromTimestamp)
			} else {
				collection := session.DB(Name).C("resume")
				doc := make(map[string]interface{})
				collection.FindId(config.ResumeName).One(doc)
				if doc["ts"] != nil {
					ts = doc["ts"].(bson.MongoTimestamp)
				}
			}
			return ts
		}
	} else if config.Replay {
		after = func(session *mgo.Session, options *gtm.Options) bson.MongoTimestamp {
			return bson.MongoTimestamp(0)
		}
	}

	var filter gtm.OpFilter = nil
	filterChain := []gtm.OpFilter{NotMongoFlux, config.onlyMeasured(), IsInsertOrUpdate}
	filter = gtm.ChainOpFilters(filterChain...)
	var oplogDatabaseName, oplogCollectionName *string
	if config.MongoOpLogDatabaseName != "" {
		oplogDatabaseName = &config.MongoOpLogDatabaseName
	}
	if config.MongoOpLogCollectionName != "" {
		oplogCollectionName = &config.MongoOpLogCollectionName
	}
	gtmBufferDuration, err := time.ParseDuration(config.GtmSettings.BufferDuration)
	if err != nil {
		log.Panicf("Unable to parse gtm buffer duration %s: %s", config.GtmSettings.BufferDuration, err)
	}
	httpConfig := client.HTTPConfig{
		UserAgent:          fmt.Sprintf("%s v%s", Name, Version),
		Addr:               config.InfluxUrl,
		Username:           config.InfluxUser,
		Password:           config.InfluxPassword,
		InsecureSkipVerify: config.InfluxSkipVerify,
	}
	if config.InfluxPemFile != "" {
		tlsConfig, err := config.InfluxTLS()
		if err != nil {
			log.Panicf("Unable to configure TLS for InfluxDB: %s", err)
		}
		httpConfig.TLSConfig = tlsConfig
	}
	influxClient, err := client.NewHTTPClient(httpConfig)
	if err != nil {
		log.Panicf("Unable to create InfluxDB client: %s", err)
	}
	var directReadNs []string
	if config.DirectReads {
		for _, m := range config.Measurement {
			directReadNs = append(directReadNs, m.Namespace)
		}
	}
	gtmCtx := gtm.Start(mongo, &gtm.Options{
		After:               after,
		NamespaceFilter:     filter,
		OpLogDatabaseName:   oplogDatabaseName,
		OpLogCollectionName: oplogCollectionName,
		ChannelSize:         config.GtmSettings.ChannelSize,
		Ordering:            gtm.Document,
		WorkerCount:         4,
		BufferDuration:      gtmBufferDuration,
		BufferSize:          config.GtmSettings.BufferSize,
		DirectReadNs:        directReadNs,
	})
	if config.DirectReads && config.ExitAfterDirectReads {
		go func() {
			gtmCtx.DirectReadWg.Wait()
			stopC <- true
		}()
	}
	exitStatus := 0
	shutdownC := make(chan bool, config.InfluxClients)
	var wg sync.WaitGroup
	for i := 1; i <= config.InfluxClients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			flusher := time.NewTicker(1 * time.Second)
			influx := &InfluxCtx{
				c:        influxClient,
				m:        make(map[string]client.BatchPoints),
				dbs:      make(map[string]bool),
				measures: make(map[string]*InfluxMeasure),
				config:   config,
				mongo:    mongo,
			}
			if err := influx.setupMeasurements(); err != nil {
				log.Panicf("Configuration error: %s", err)
			}
			for {
				select {
				case <-shutdownC:
					if err := influx.writeBatch(); err != nil {
						exitStatus = 1
						log.Println(err)
					}
					return
				case <-flusher.C:
					if err := influx.writeBatch(); err != nil {
						gtmCtx.ErrC <- err
					}
				case err = <-gtmCtx.ErrC:
					exitStatus = 1
					log.Println(err)
				case op := <-gtmCtx.OpC:
					if err := influx.addPoint(op); err != nil {
						gtmCtx.ErrC <- err
					}
				}
			}
		}()
	}
	<-stopC
	if config.Verbose {
		infoLog.Println("flushing in-flight points")
	}
	for i := 1; i <= config.InfluxClients; i++ {
		shutdownC <- true
	}
	wg.Wait()
	mongo.Close()
	influxClient.Close()
	os.Exit(exitStatus)
}
