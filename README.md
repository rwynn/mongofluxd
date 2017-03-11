# mongofluxd
Real time sync from MongoDB into InfluxDB

### Installation

Download the latest [release](https://github.com/rwynn/mongofluxd/releases) or install with go get

	go get -v github.com/rwynn/mongofluxd

### Usage

Since mongofluxd uses the mongodb oplog it is required that MongoDB is configured to produce an oplog.

This can be ensured by doing one of the following:
+ Setting up [replica sets](http://docs.mongodb.org/manual/tutorial/deploy-replica-set/)
+ Passing --master to the mongod process
+ Setting the following in /etc/mongod.conf

	```
	master = true
	```

Run mongofluxd with the -f option to point to a configuration file.  The configuration format is toml.

A configuration looks like this:

```toml
influx-url = "http://localhost:8086"
influx-skip-verify = true
influx-auto-create-db = true
#influx-pem-file = "/path/to/cert.pem"
influx-clients = 10

mongo-url = "localhost"
# use the default MongoDB port on localhost
mongo-skip-verify = true
#mongo-pem-file = "/path/to/cert.pem"

replay = false
# process all events from the beginning of the oplog

resume = false
# save the timestamps of processed events for resuming later

resume-name = "mongofluxd"
# the key to store timestamps under in the collection mongoflux.resume

verbose = false
# output some information when points are written

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
```

### Some numbers

Load 100K documents of time series data into MongoDB.

        // sleep for 2ms to ensure t is 2ms apart
	for (var i=0; i<100000; ++i) { sleep(2); var t = new Date(); db.test.insert({c: 1, d: 5.5, t: t}); }

Run monfluxd with direct reads on test.test (config contents above)

	time ./mongofluxd -f flux.toml

	real    0m2.167s
	user    0m2.028s
	sys     0m0.444s

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
took only 2.167 seconds for a throughput of 46,146 points per second.

