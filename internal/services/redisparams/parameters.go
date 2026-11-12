// Package redisparams defines shared Redis and Valkey engine parameters.
package redisparams

const (
	typeString  = "string"
	typeInteger = "integer"
	minVerRedis = "6.0.0"
)

type Definition struct {
	Name          string
	Description   string
	DataType      string
	AllowedValues string
	DefaultValue  string
	MinEngineVer  string
	IsModifiable  bool
	Dynamic       bool
}

var Redis = []Definition{
	{
		Name:        "maxmemory-policy",
		Description: "How Redis will select what to remove when maxmemory is reached",
		DataType:    typeString,
		AllowedValues: "volatile-lru,allkeys-lru,volatile-lfu,allkeys-lfu,volatile-random," +
			"allkeys-random,volatile-ttl,noeviction",
		DefaultValue: "volatile-lru",
		IsModifiable: true,
		Dynamic:      true,
		MinEngineVer: minVerRedis,
	},
	{
		Name:          "timeout",
		Description:   "Close connection after client is idle for N seconds (0 to disable)",
		DataType:      typeInteger,
		AllowedValues: "0-31536000",
		DefaultValue:  "0",
		IsModifiable:  true,
		Dynamic:       true,
		MinEngineVer:  minVerRedis,
	},
	{
		Name:          "notify-keyspace-events",
		Description:   "Events to notify via Pub/Sub",
		DataType:      typeString,
		AllowedValues: "K,E,g,$,l,s,h,z,x,e,A,t,m",
		DefaultValue:  "",
		IsModifiable:  true,
		Dynamic:       true,
		MinEngineVer:  minVerRedis,
	},
	{
		Name:          "databases",
		Description:   "Set the number of databases",
		DataType:      typeInteger,
		AllowedValues: "1-1000000",
		DefaultValue:  "16",
		IsModifiable:  true,
		Dynamic:       false,
		MinEngineVer:  minVerRedis,
	},
	{
		Name:          "appendonly",
		Description:   "Enable append only mode",
		DataType:      typeString,
		AllowedValues: yesNo,
		DefaultValue:  "no",
		IsModifiable:  true,
		Dynamic:       true,
		MinEngineVer:  minVerRedis,
	},
}
