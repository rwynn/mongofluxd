module github.com/rwynn/mongofluxd

require (
	github.com/BurntSushi/toml v0.3.1
	github.com/globalsign/mgo v0.0.0-20181015135952-eeefdecb41b8
	github.com/influxdata/influxdb v1.7.3
	github.com/influxdata/platform v0.0.0-20190117200541-d500d3cf5589 // indirect
	github.com/pkg/errors v0.8.1 // indirect
	github.com/rwynn/gtm v0.0.0-20190131014030-4d96eedfb073
	github.com/serialx/hashring v0.0.0-20180504054112-49a4782e9908 // indirect
)

replace github.com/globalsign/mgo v0.0.0-20181015135952-eeefdecb41b8 => github.com/rwynn/mgo v0.0.0-20190203195949-55e8fd85e7e2
