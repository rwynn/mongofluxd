package mongofluxdplug

import "time"

// plugins must import this package
// import "github.com/rwynn/mongofluxd/mongofluxdplug

// plugins must implement a function per measurement with the following signature
// e.g. func MyPointMapper(input *mongofluxdplug.MongoDocument) (output []*mongofluxdplug.InfluxPoint, err error)
// the function name must then be associated with the measurement in the toml config
// [[measurement]]
// symbol = "MyPointMapper"

// plugins can be compiled using go build -buildmode=plugin -o myplugin.so myplugin.go
// to enable the plugin start with mongofluxd -plugin-path /path/to/myplugin.so

type MongoDocument struct {
	Data       map[string]interface{} // the original document data from MongoDB
	Database   string                 // the origin database in MongoDB
	Collection string                 // the origin collection in MongoDB
	Namespace  string                 // the entire namespace for the original document
	Operation  string                 // "i" for a insert or "u" for update
}

type InfluxPoint struct {
	Tags      map[string]string      // optional tags to set on the Point
	Fields    map[string]interface{} // fields to set on the Point
	Timestamp time.Time              // the time of the Point
}
