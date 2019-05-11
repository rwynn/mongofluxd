# mongofluxd
Real time sync from MongoDB into InfluxDB

### Requirements

This tool supports MongoDB 3.6+ and InfluxDB 1.X. The go driver for InfluxDB 2.X has not yet been released so this
tool has only been tested to work with InfluxDB 1.X.  

### Installation

Download the latest [release](https://github.com/rwynn/mongofluxd/releases) or install with golang 1.11 and above

	# clone the repo outside your $GOPATH
	git clone https://github.com/rwynn/mongofluxd.git
	cd mongofluxd
	# install binary to $GOPATH/bin/mongofluxd
	go install

### Usage

Mongofluxd uses the MongoDB [oplog](https://docs.mongodb.com/manual/core/replica-set-oplog/) as an event source. 

You will need to ensure that MongoDB is configured to produce an oplog by 
[deploying a replica set](http://docs.mongodb.org/manual/tutorial/deploy-replica-set/).

If you haven't already done so, follow the 5 step 
[procedure](https://docs.mongodb.com/manual/tutorial/deploy-replica-set/#procedure) 
to initiate and validate your replica set. For local testing your replica set may contain a 
[single member](https://docs.mongodb.com/manual/tutorial/convert-standalone-to-replica-set/).

Run mongofluxd with the -f option to point to a configuration file.  The configuration format is toml.

A configuration looks like this:

```toml
influx-url = "http://localhost:8086"
influx-skip-verify = true
influx-auto-create-db = true
#influx-pem-file = "/path/to/cert.pem"
influx-clients = 10

mongo-url = "mongodb://localhost:27017"
# use the default MongoDB port on localhost
# see https://github.com/mongodb/mongo-go-driver/blob/master/x/network/connstring/connstring.go for all options

replay = false
# process all events from the beginning of the oplog

resume = false
# save the timestamps of processed events for resuming later

resume-name = "mongofluxd"
# the key to store timestamps under in the collection mongoflux.resume

verbose = false
# output some information when points are written

change-streams = true
# you should turn on change streams but only if using MongoDB 3.6+

direct-reads = true
# read events directly out of mongodb collections in addition to tailing the oplog

exit-after-direct-reads = true
# exit the process after direct reads have completed. defaults to false to continuously read events from the oplog

[[measurement]]
# this measurement will only apply to the collection test in db test
# measurements are stored in an Influx DB matching the name of the MongoDB database
namespace = "test.test"
# fields must be document properties of type int, float, bool, or string
# nested fields like "e.f" are supported, e.g. { e: { f: 1.5 }}
fields = ["c", "d"]
# optionally override the field to take time from.  defaults to the insertion ts at second precision
# recommended if you need ms precision.  use Mongo's native Date object to get ms precision
timefield = "t"
# optionally override the time precision.  defaults to "s" since MongoDB oplog entries are to the second
# use in conjunction with timefield and native Mongo Date to get ms precision
precision = "ms"

[[measurement]]
namespace = "db.products"
# optional tags must be top level document properties with string values
tags = ["sku", "category"]
fields = ["sales", "price"]
# set the retention policy for this measurement
retention = "RP1" 
# override the measurement name which defaults to the name of the MongoDB collection
measure = "sales"
# the measurement name can be calculated from the Fields, Tags, or Doc in a golang template
# measure = "{{.Tags.category}}_{{.Fields.price}}_{{.Doc.name}}"
# override the influx database name which default to the name of the MongoDB database
database = "salesdb"

[[measurement]]
namespace = "db.col"
# You can specify a view of the namespace.  Direct reads will go through the view.
# Change docs for db.col will also be routed through the view. The _id of the doc
# that changed is used as the key into the view.
view = "db.viewofcol"
```

### Some numbers

Load 100K documents of time series data into MongoDB.

	// sleep for 1ms to ensure t is 1ms apart
	for (var i=0; i<100000; ++i) { sleep(1); var t = new Date(); db.test.insert({c: 1, d: 5.5, t: t}); }

Run monfluxd with direct reads on test.test (config contents above)

	time ./mongofluxd -f ~/INFLUX.toml 
	INFO 2018/03/17 14:58:02 Direct read parallel collection scan is ON
	INFO 2018/03/17 14:58:02 Parallel collection scan command returned 7/10 cursors requested for test.test
	INFO 2018/03/17 14:58:02 Starting 7 go routines to read test.test

	real	0m1.432s
	user	0m2.520s
	sys	0m0.672s

Verify it all got into InfluxDB

	Connected to http://localhost:8086 version 1.2.0
	InfluxDB shell version: 1.2.0
	> use test;
	Using database test
	> select count(*) from test;
	name: test
	time count_c count_d
	---- ------- -------
	0    100000  100000


On a VirtualBox VM with 4 virtual cores and 4096 mb of memory, syncing 100K documents from MongoDB to InfluxDB
took only 1.432 seconds for a throughput of ~ 70K points per second.

### Advanced

mongofluxd supports golang 1.8 plugins for advanced use cases. For example, you have a one-to-many relationship
between MongoDB documents and InfluxDB points.  mongofluxd supports consuming 1 go plugin .so.  This .so may expose
many public functions.  mongofluxd supports mapping one plugin symbol (a function) with each measurement.

The mapping function must be of the form:

	func (*mongofluxdplug.MongoDocument) ([]*mongofluxdplug.InfluxPoint, error)

The following example plugin maps a single MongoDB document to multiple Points in InfluxDB:

```go
package main

import (
	"fmt"
	"github.com/rwynn/mongofluxd/mongofluxdplug"
	"time"
)

// Plugin to map a single MongoDB document to multiple InfluxDB points
//
// e.g.
// db.testplug.insert({ts: new Date(), pts: [{o: 0, d: 1.5}, {o: 2, d: 3.2}]})
// where ts is the base time, o is the second offset for each point, and d is field data for each point

func MyPointMapper(input *mongofluxdplug.MongoDocument) (output []*mongofluxdplug.InfluxPoint, err error) {
	doc := input.Data

	// reference base time
	var t time.Time
	switch doc["ts"].(type) {
	case time.Time:
		t = doc["ts"].(time.Time)
	default:
		return nil, fmt.Errorf("expected ts field with type %T but got %T", t, doc["ts"])
	}

	// reference list of points with time offset and data
	var pts []interface{}
	switch doc["pts"].(type) {
	case []interface{}:
		pts = doc["pts"].([]interface{})
	default:
		return nil, fmt.Errorf("expected pts field with type %T but got %T", pts, doc["pts"])
	}

	// for each pt in this single document, add an InfluxPoint to the output
	for _, p := range pts {

		// assert type of each point p
		var pt map[string]interface{}
		switch ptt := p.(type) {
		case map[string]interface{}:
			pt = ptt
		default:
			return nil, fmt.Errorf("expected point of type %T but got %T", pt, p)
		}

		// read offset and point data
		var offset, pointData float64
		switch pt["o"].(type) {
		case float64:
			offset = pt["o"].(float64)
		default:
			return nil, fmt.Errorf("expected offset of type %T but got %T", offset, pt["o"])
		}
		switch pt["d"].(type) {
		case float64:
			pointData = pt["d"].(float64)
		default:
			return nil, fmt.Errorf("expected point data of type %T but got %T", pointData, pt["d"])
		}

		// create a new InfluxPoint
		point := &mongofluxdplug.InfluxPoint{
			Tags:   make(map[string]string),
			Fields: make(map[string]interface{}),
		}

		// set time, fields, and tags on the Point
		point.Timestamp = t.Add(time.Duration(int64(offset)) * time.Second)
		point.Fields["d"] = pointData

		// append the Point to the output
		output = append(output, point)
	}
	return output, nil
}
```
To build a plugin you must use golang 1.11 and above and ensure you run the `go build` command with
the mongofluxd `go.mod` file in the current directory. This is to ensure your plugin dependencies use 
the exact same source code as mongofluxd.

	# clone the repo outside your $GOPATH
	git clone https://github.com/rwynn/mongofluxd.git
	cd mongofluxd
	# add and edit a file myplugin.go which is your plugin
	go build -buildmode=plugin -o myplugin.so myplugin.go

The public plugin function, or symbol, can then be assigned to a measurement in the config file

```toml

plugin-path = "/path/to/myplugin.so"

[[measurement]]
namespace = "test.testplug"
# for this measurement use a go plugin to map a single MongoDB document to multiple InfluxDB points
# in this case the function name to use is MyPointMapper
# the time, fields, and tags will be generated by the plugin
symbol = "MyPointMapper"
precision = "ms"
```

When a MongoDB document is inserted into the `test.testplug` namespace, the `MyPointMapper` function will
be invoked to determine a slice of Points to write to InfluxDB.

