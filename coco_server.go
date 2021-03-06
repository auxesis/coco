package main

import (
	"github.com/BurntSushi/toml"
	"github.com/bulletproofnetworks/coco/coco"
	collectd "github.com/kimor79/gollectd"
	"gopkg.in/alecthomas/kingpin.v1"
	"log"
)

var (
	configPath = kingpin.Arg("config", "Path to coco config").Default("coco.conf").String()
)

func main() {
	kingpin.Version("1.0.0")
	kingpin.Parse()

	var config coco.Config
	if _, err := toml.DecodeFile(*configPath, &config); err != nil {
		log.Fatalln("fatal:", err)
		return
	}

	// Setup data structures to be shared across components
	blacklisted := map[string]map[string]int64{}
	raw := make(chan collectd.Packet, 1000000)
	filtered := make(chan collectd.Packet, 1000000)
	items := make(chan coco.BlacklistItem, 1000000)

	var tiers []coco.Tier
	for k, v := range config.Tiers {
		tier := coco.Tier{Name: k, Targets: v.Targets}
		tiers = append(tiers, tier)
	}

	if len(tiers) == 0 {
		log.Fatal("No tiers configured. Exiting.")
	}

	chans := map[string]chan collectd.Packet{
		"raw":      raw,
		"filtered": filtered,
		//"blacklist_items": items,
	}
	go coco.Measure(config.Measure, chans, &tiers)

	// Launch components to do the work
	go coco.Listen(config.Listen, raw)
	for i := 0; i < 4; i++ {
		go coco.Filter(config.Filter, raw, filtered, items)
	}
	go coco.Blacklist(items, &blacklisted)
	go coco.Send(&tiers, filtered)
	coco.Api(config.Api, &tiers, &blacklisted)
}
