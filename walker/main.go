package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	"github.com/peterbourgon/g2s"
	"github.com/soundcloud/roshi/cluster"
	"github.com/soundcloud/roshi/farm"
	"github.com/soundcloud/roshi/instrumentation"
	"github.com/soundcloud/roshi/instrumentation/statsd"
	"github.com/soundcloud/roshi/shard"
	"github.com/tsenart/tb"
)

func main() {
	var (
		redisInstances      = flag.String("redis.instances", "", "Semicolon-separated list of comma-separated lists of Redis instances")
		redisConnectTimeout = flag.Duration("redis.connect.timeout", 3*time.Second, "Redis connect timeout")
		redisReadTimeout    = flag.Duration("redis.read.timeout", 3*time.Second, "Redis read timeout")
		redisWriteTimeout   = flag.Duration("redis.write.timeout", 3*time.Second, "Redis write timeout")
		redisMCPI           = flag.Int("redis.mcpi", 10, "Max connections per Redis instance")
		redisHash           = flag.String("redis.hash", "murmur3", "Redis hash function: murmur3, fnv, fnva")
		maxSize             = flag.Int("max.size", 10000, "Maximum number of events per key")
		batchSize           = flag.Int("batch.size", 100, "keys to select per request")
		maxKeysPerSecond    = flag.Int64("max.keys.per.second", 1000, "max keys per second to walk")
		once                = flag.Bool("once", false, "walk entire keyspace once and exit (default false, walk forever)")
		statsdAddress       = flag.String("statsd.address", "", "Statsd address (blank to disable)")
		statsdSampleRate    = flag.Float64("statsd.sample.rate", 0.1, "Statsd sample rate for normal metrics")
		statsdBucketPrefix  = flag.String("statsd.bucket.prefix", "myservice.", "Statsd bucket key prefix, including trailing period")
	)
	flag.Parse()

	// Validate integer arguments.
	if *maxKeysPerSecond < int64(*batchSize) {
		log.Fatal("max keys per second should be bigger than batch size")
	}

	// Set up statsd instrumentation, if it's specified.
	stats := g2s.Noop()
	if *statsdAddress != "" {
		var err error
		stats, err = g2s.Dial("udp", *statsdAddress)
		if err != nil {
			log.Fatal(err)
		}
	}
	instr := statsd.New(stats, float32(*statsdSampleRate), *statsdBucketPrefix)

	// Parse hash function.
	var hashFunc func(string) uint32
	switch strings.ToLower(*redisHash) {
	case "murmur3":
		hashFunc = shard.Murmur3
	case "fnv":
		hashFunc = shard.FNV
	case "fnva":
		hashFunc = shard.FNVa
	default:
		log.Fatalf("unknown hash '%s'", *redisHash)
	}

	// Set up the clusters.
	clusters, err := makeClusters(
		*redisInstances,
		*redisConnectTimeout, *redisReadTimeout, *redisWriteTimeout,
		*redisMCPI,
		hashFunc,
		*maxSize,
		instr,
	)
	if err != nil {
		log.Fatal(err)
	}

	// Set up our rate limiter. Remember: it's per-key, not per-request.
	throttle := newThrottle(*maxKeysPerSecond)

	// Perform the walk
	dst := farm.New(clusters, len(clusters), farm.SendAllReadAll, farm.AllRepairs(clusters, instr), instr)
	for {
		src := scan(clusters) // new key set
		walk(dst, throttle, src, *batchSize, *maxSize, instr)
		if *once {
			return
		}
	}
}

func makeClusters(
	redisInstances string,
	connectTimeout, readTimeout, writeTimeout time.Duration,
	redisMCPI int,
	hashFunc func(string) uint32,
	maxSize int,
	instr instrumentation.Instrumentation,
) ([]cluster.Cluster, error) {
	clusters := []cluster.Cluster{}
	for i, clusterInstances := range strings.Split(redisInstances, ";") {
		addresses := stripBlank(strings.Split(clusterInstances, ","))
		if len(addresses) <= 0 {
			continue
		}
		clusters = append(clusters, cluster.New(
			shard.New(
				addresses,
				connectTimeout, readTimeout, writeTimeout,
				redisMCPI,
				hashFunc,
			),
			maxSize,
			instr,
		))
		log.Printf("Redis cluster %d: %d instance(s)", i+1, len(addresses))
	}
	if len(clusters) <= 0 {
		return []cluster.Cluster{}, fmt.Errorf("no cluster(s)")
	}
	return clusters, nil
}

func scan(clusters []cluster.Cluster) <-chan string {
	c := make(chan string)
	go func() {
		defer close(c)
		order := rand.Perm(len(clusters))
		for _, index := range order {
			for key := range clusters[index].Keys() {
				c <- key
			}
		}
	}()
	return c
}

func walk(dst farm.Selecter, throttle *throttle, src <-chan string, batchSize, maxSize int, instr instrumentation.WalkInstrumentation) {
	batch := []string{}
	waitSelectReset := func() {
		throttle.wait(int64(len(batch)))
		dst.Select(batch, 0, maxSize)
		instr.WalkKeys(len(batch))
		batch = []string{}
	}
	for key := range src {
		batch = append(batch, key)
		if len(batch) >= batchSize {
			waitSelectReset()
		}
	}
	waitSelectReset()
}

type throttle struct {
	bucket       *tb.Bucket
	waitInterval time.Duration
}

func newThrottle(maxPerSecond int64) *throttle {
	return &throttle{
		bucket:       tb.NewBucket(maxPerSecond, -1),
		waitInterval: (1 * time.Second) / time.Duration(maxPerSecond),
	}
}

func (t *throttle) wait(n int64) {
	got := t.bucket.Take(n)
	for got < n {
		time.Sleep(t.waitInterval)
		got += t.bucket.Take(n - got)
	}
}

func stripBlank(src []string) []string {
	dst := []string{}
	for _, s := range src {
		if s == "" {
			continue
		}
		dst = append(dst, s)
	}
	return dst
}
