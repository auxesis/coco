package coco

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"expvar"
	"fmt"
	"github.com/go-martini/martini"
	collectd "github.com/kimor79/gollectd"
	consistent "github.com/stathat/consistent"
	"log"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

func Measure(config MeasureConfig, chans map[string]chan collectd.Packet) {
	tick := time.NewTicker(config.Interval()).C
	for n, _ := range chans {
		log.Println("info: measure: measuring queue", n)
		queueCounts.Set(n, &expvar.Int{})
	}
	for {
		select {
		case <-tick:
			for n, c := range chans {
				queueCounts.Get(n).(*expvar.Int).Set(int64(len(c)))
			}
		}
	}
}

// Listen for collectd network packets, parse , and send them over a channel
func Listen(config ListenConfig, c chan collectd.Packet) {
	// Initialise the error counts
	errorCounts.Add("fetch.receive", 0)

	laddr, err := net.ResolveUDPAddr("udp", config.Bind)
	if err != nil {
		log.Fatalln("fatal: failed to resolve address", err)
	}

	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		log.Fatalln("fatal: failed to listen", err)
	}

	types, err := collectd.TypesDBFile(config.Typesdb)
	if err != nil {
		log.Fatalln("fatal: failed to parse types.db", err)
	}

	for {
		// 1452 is collectd 5's default buffer size. See:
		// https://collectd.org/wiki/index.php/Binary_protocol
		buf := make([]byte, 1452)

		n, err := conn.Read(buf[:])
		if err != nil {
			log.Println("error: Failed to receive packet", err)
			errorCounts.Add("fetch.receive", 1)
			continue
		}
		listenCounts.Add("raw", 1)

		packets, err := collectd.Packets(buf[0:n], types)
		for _, p := range *packets {
			listenCounts.Add("decoded", 1)
			c <- p
		}
	}
}

func MetricName(packet collectd.Packet) string {
	prts := []string{
		packet.Plugin,
		packet.PluginInstance,
		packet.Type,
		packet.TypeInstance,
	}

	var parts []string

	for _, p := range prts {
		if len(p) != 0 {
			parts = append(parts, p)
		}
	}

	return strings.Join(parts, "/")
}

func Filter(config FilterConfig, raw chan collectd.Packet, filtered chan collectd.Packet, mapping map[string]map[string]map[string]int64) {
	// Initialise the error counts
	errorCounts.Add("filter.unhandled", 0)

	// Track unhandled errors
	defer func() {
		if r := recover(); r != nil {
			errorCounts.Add("filter.unhandled", 1)
		}
	}()

	mapping["filtered"] = make(map[string]map[string]int64)
	for {
		packet := <-raw
		name := MetricName(packet)
		full := packet.Hostname + "/" + name

		re := regexp.MustCompile(config.Blacklist)
		if re.FindStringIndex(full) == nil {
			filtered <- packet
			filterCounts.Add("accepted", 1)
		} else {
			if mapping["filtered"][packet.Hostname] == nil {
				mapping["filtered"][packet.Hostname] = make(map[string]int64)
			}
			mapping["filtered"][packet.Hostname][name] = time.Now().Unix()
			filterCounts.Add("rejected", 1)
		}
	}
}

func Send(tiers *[]Tier, filtered chan collectd.Packet, mapping map[string]map[string]map[string]int64) {
	/*
		ratioCounts.Set("tier1", new(expvar.Map).Init())
		ratioCounts.Set("tier2", new(expvar.Map).Init())

		ratioCountsMap := new(expvar.Map).Init()
		i := new(expvar.Int)
		i.Set(1)
		ratioCountsMap.Set("min", i)
		ratioCountsMap.Set("max", &expvar.Int{})
		ratioCounts.Set("tier1", ratioCountsMap)
	*/

	//metricCounts.Get(target).(*expvar.Int).Set(int64(mc))

	// Initialise the error counts
	errorCounts.Add("send.dial", 0)
	errorCounts.Add("send.write", 0)

	// map that tracks all the UDP connections
	connections := make(map[string]net.Conn)

	for i, tier := range *tiers {
		(*tiers)[i].Hash = consistent.New()
		(*tiers)[i].Shadows = make(map[string]string)
		// Populate ratio counters per tier
		ratioCounts.Set(tier.Name, new(expvar.Map).Init())

		for it, t := range tier.Targets {
			conn, err := net.Dial("udp", t)
			if err != nil {
				log.Printf("error: send: %s: %+v", t, err)
				errorCounts.Add("send.dial", 1)
			} else {
				// Only add the target to the hash if the connection can initially be established
				re := regexp.MustCompile("^(127.|localhost)")
				if re.FindStringIndex(conn.RemoteAddr().String()) != nil {
					log.Printf("warning: %s is local. You may be looping metrics back to coco!", conn.RemoteAddr())
					log.Printf("warning: send dutifully adding %s to hash anyway, but beware!", conn.RemoteAddr())
				}
				mapping[t] = make(map[string]map[string]int64)
				connections[t] = conn
				shadow_t := string(it)
				(*tiers)[i].Shadows[shadow_t] = t
				(*tiers)[i].Hash.Add(shadow_t)
				metricCounts.Set(t, &expvar.Int{})
				hostCounts.Set(t, &expvar.Int{})
			}
		}
	}

	// Log how the hashes are set up
	for _, tier := range *tiers {
		hash := tier.Hash
		var targets []string
		for _, shadow_t := range hash.Members() {
			targets = append(targets, tier.Shadows[shadow_t])
		}
		log.Printf("info: send: tier %s hash ring has %d members: %s", tier.Name, len(hash.Members()), targets)
	}

	if len(connections) == 0 {
		log.Fatal("fatal: send: no targets in any hash ring in any tier")
	}

	for {
		packet := <-filtered
		for _, tier := range *tiers {
			// Get the target we should forward the packet to
			shadow_t, err := tier.Hash.Get(packet.Hostname)
			if err != nil {
				log.Fatal(err)
			}
			target := tier.Shadows[shadow_t]

			// Update metadata
			name := MetricName(packet)
			if mapping[target][packet.Hostname] == nil {
				mapping[target][packet.Hostname] = make(map[string]int64)
			}
			mapping[target][packet.Hostname][name] = time.Now().Unix()

			// Dispatch the metric
			payload := Encode(packet)
			_, err = connections[target].Write(payload)
			if err != nil {
				errorCounts.Add("send.write", 1)
				continue
			}

			// Update counters
			hostCounts.Get(target).(*expvar.Int).Set(int64(len(mapping[target])))
			mc := 0
			for _, v := range mapping[target] {
				mc += len(v)
			}
			metricCounts.Get(target).(*expvar.Int).Set(int64(mc))
			sendCounts.Add(target, 1)
			sendCounts.Add("total", 1)

			// Update totals
			var min int
			var max int
			var sum int
			var length int
			var n int
			// Find min + max
			for t, hosts := range mapping {
				if t == "filtered" {
					continue
				}
				sizes := []int{}
				for _, metrics := range hosts {
					sizes = append(sizes, len(metrics))
					sum += len(metrics)
				}
				sort.Ints(sizes)
				min = sizes[0]
				max = sizes[len(sizes)-1]
				length = len(sizes)
				n = sizes[int(float64(length)*0.95)]
			}
			// Build summary
			ratioCountsMap := new(expvar.Map).Init()
			mine := new(expvar.Int)
			mine.Set(int64(min))
			ratioCountsMap.Set("min", mine)
			maxe := new(expvar.Int)
			maxe.Set(int64(max))
			ratioCountsMap.Set("max", maxe)
			avge := new(expvar.Float)
			avge.Set(float64(sum) / float64(length))
			ratioCountsMap.Set("avg", avge)
			ne := new(expvar.Int)
			ne.Set(int64(n))
			ratioCountsMap.Set("95e", ne)
			ratioCounts.Set(tier.Name, ratioCountsMap)
		}
	}
}

// Encode a Packet into the collectd wire protocol format.
func Encode(packet collectd.Packet) []byte {
	// String parts have a length of 5, because there is a terminating null byte
	// Numeric parts have a length of 4, because there is no terminating null byte

	buf := make([]byte, 0)
	null := []byte("\x00")

	// Hostname - String part
	hostBytes := []byte(packet.Hostname)
	buf = append(buf, []byte{0, collectd.ParseHost}...)
	buf = append(buf, []byte{0, byte(len(hostBytes) + 5)}...)
	buf = append(buf, hostBytes...)
	buf = append(buf, null...) // null bytes for string parts

	// Time - Number part
	if packet.Time > 0 {
		timeBytes := new(bytes.Buffer)
		binary.Write(timeBytes, binary.BigEndian, packet.Time)
		buf = append(buf, []byte{0, collectd.ParseTime}...)
		buf = append(buf, []byte{0, byte(len(timeBytes.Bytes()) + 4)}...)
		buf = append(buf, timeBytes.Bytes()...)
	}

	// TimeHR - Number part
	if packet.TimeHR > 0 {
		timeHRBytes := new(bytes.Buffer)
		binary.Write(timeHRBytes, binary.BigEndian, packet.TimeHR)
		buf = append(buf, []byte{0, collectd.ParseTimeHR}...)
		buf = append(buf, []byte{0, byte(len(timeHRBytes.Bytes()) + 4)}...)
		buf = append(buf, timeHRBytes.Bytes()...)
	}

	// Interval - Number part
	if packet.Interval > 0 {
		intervalBytes := new(bytes.Buffer)
		binary.Write(intervalBytes, binary.BigEndian, packet.Interval)
		buf = append(buf, []byte{0, collectd.ParseInterval}...)
		buf = append(buf, []byte{0, byte(len(intervalBytes.Bytes()) + 4)}...)
		buf = append(buf, intervalBytes.Bytes()...)
	}

	if packet.IntervalHR > 0 {
		intervalHRBytes := new(bytes.Buffer)
		binary.Write(intervalHRBytes, binary.BigEndian, packet.IntervalHR)
		buf = append(buf, []byte{0, collectd.ParseIntervalHR}...)
		buf = append(buf, []byte{0, byte(len(intervalHRBytes.Bytes()) + 4)}...)
		buf = append(buf, intervalHRBytes.Bytes()...)
	}

	// Plugin - String part
	pluginBytes := []byte(packet.Plugin)
	buf = append(buf, []byte{0, collectd.ParsePlugin}...)
	buf = append(buf, []byte{0, byte(len(pluginBytes) + 5)}...)
	buf = append(buf, pluginBytes...)
	buf = append(buf, null...) // null bytes for string parts

	// PluginInstance - String part
	if len(packet.PluginInstance) > 0 {
		pluginInstanceBytes := []byte(packet.PluginInstance)
		buf = append(buf, []byte{0, collectd.ParsePluginInstance}...)
		buf = append(buf, []byte{0, byte(len(pluginInstanceBytes) + 5)}...)
		buf = append(buf, pluginInstanceBytes...)
		buf = append(buf, null...) // null bytes for string parts
	}

	// Type - String part
	typeBytes := []byte(packet.Type)
	buf = append(buf, []byte{0, collectd.ParseType}...)
	buf = append(buf, []byte{0, byte(len(typeBytes) + 5)}...)
	buf = append(buf, typeBytes...)
	buf = append(buf, null...) // null bytes for string parts

	// TypeInstance - String part
	if len(packet.TypeInstance) > 0 {
		typeInstanceBytes := []byte(packet.TypeInstance)
		buf = append(buf, []byte{0, collectd.ParseTypeInstance}...)
		buf = append(buf, []byte{0, byte(len(typeInstanceBytes) + 5)}...)
		buf = append(buf, typeInstanceBytes...)
		buf = append(buf, null...) // null bytes for string parts
	}

	// Values - Values part
	valuesBuf := make([]byte, 0)

	// Write out the types
	for _, v := range packet.Values {
		valueTypeBytes := new(bytes.Buffer)
		binary.Write(valueTypeBytes, binary.BigEndian, v.Type)
		valuesBuf = append(valuesBuf, valueTypeBytes.Bytes()...)
	}

	// Then write out the values
	for _, v := range packet.Values {
		if v.Type == collectd.TypeGauge {
			gaugeBytes := new(bytes.Buffer)
			binary.Write(gaugeBytes, binary.LittleEndian, v.Value)
			valuesBuf = append(valuesBuf, gaugeBytes.Bytes()...)
		} else {
			valueBytes := new(bytes.Buffer)
			binary.Write(valueBytes, binary.BigEndian, v.Value)
			valuesBuf = append(valuesBuf, valueBytes.Bytes()...)
		}
	}

	// type(2) + length(2) + number of values(2) == 6
	buf = append(buf, []byte{0, collectd.ParseValues}...)     // type
	buf = append(buf, []byte{0, byte(len(valuesBuf) + 6)}...) // length
	buf = append(buf, []byte{0, byte(len(packet.Values))}...) // number of values
	buf = append(buf, valuesBuf...)                           // values themselves

	return buf
}

func TierLookup(params martini.Params, req *http.Request, tiers *[]Tier) []byte {
	// Initialise the error counts
	errorCounts.Add("lookup.hash.get", 0)

	qs := req.URL.Query()
	if len(qs["name"]) > 0 {
		name := qs["name"][0]
		result := map[string]string{}

		for _, tier := range *tiers {
			shadow_t, err := tier.Hash.Get(name)
			if err != nil {
				defer func() {
					errorCounts.Add("lookup.hash.get", 1)
					log.Printf("error: api: %s: %+v\n", name, err)
				}()
			}
			target := tier.Shadows[shadow_t]
			// Track when we've successfully looked up a tier.
			defer func() {
				lookupCounts.Add(tier.Name, 1)
			}()
			result[tier.Name] = target
		}
		json, _ := json.Marshal(result)
		return json
	} else {
		return []byte("")
	}
}

func ExpvarHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	fmt.Fprintf(w, "{")
	first := true
	systems := map[string]map[string]interface{}{}
	systems["coco"] = make(map[string]interface{})
	systems["noodle"] = make(map[string]interface{})

	expvar.Do(func(kv expvar.KeyValue) {
		re := regexp.MustCompile("^(coco|noodle)")
		if re.FindStringIndex(kv.Key) != nil {
			parts := strings.SplitN(kv.Key, ".", 2)
			sys := parts[0]
			key := parts[1]
			systems[sys][key] = kv.Value
		} else {
			if !first {
				fmt.Fprintf(w, ",")
			}
			first = false
			fmt.Fprintf(w, "%q: %s", kv.Key, kv.Value)
		}
	})

	for k, _ := range systems {
		first = true
		fmt.Fprintf(w, ",%q: {", k)
		for k, v := range systems[k] {
			if !first {
				fmt.Fprintf(w, ",")
			}
			first = false
			fmt.Fprintf(w, "%q:%s", k, v)
		}
		fmt.Fprintf(w, "}")
	}

	fmt.Fprintf(w, "}\n")
}

func Api(config ApiConfig, tiers *[]Tier, mapping map[string]map[string]map[string]int64) {
	m := martini.Classic()
	// Endpoint for looking up what storage nodes own metrics for a host
	m.Get("/lookup", func(params martini.Params, req *http.Request) []byte {
		return TierLookup(params, req, tiers)
	})
	// Dump out the list of targets Coco is hashing metrics to
	m.Group("/targets", func(r martini.Router) {
		r.Get("", func() []byte {
			data, _ := json.Marshal(mapping)
			return data
		})
	})
	// Implement expvars.expvarHandler in Martini.
	m.Get("/debug/vars", func(w http.ResponseWriter, r *http.Request) {
		ExpvarHandler(w, r)
	})

	log.Printf("info: binding web server to %s", config.Bind)
	log.Fatal(http.ListenAndServe(config.Bind, m))
}

type Config struct {
	Listen  ListenConfig
	Filter  FilterConfig
	Tiers   map[string]TierConfig
	Api     ApiConfig
	Fetch   FetchConfig
	Measure MeasureConfig
}

type ListenConfig struct {
	Bind    string
	Typesdb string
}

type FilterConfig struct {
	Blacklist string
}

type TierConfig struct {
	Targets []string
}

type ApiConfig struct {
	Bind string
}

type FetchConfig struct {
	Bind         string
	ProxyTimeout Duration `toml:"proxy_timeout"`
	// FIXME(lindsay): RemotePort is a bit of a code smell.
	// Ideally every target could define its own port for collectd + Visage.
	RemotePort string `toml:"remote_port"`
}

// Helper function to provide a default timeout value
func (f *FetchConfig) Timeout() time.Duration {
	if f.ProxyTimeout.Duration == 0 {
		return 3 * time.Second
	} else {
		return f.ProxyTimeout.Duration
	}
}

type MeasureConfig struct {
	TickInterval Duration `toml:"interval"`
}

// Helper function to provide a default interval value
func (m *MeasureConfig) Interval() time.Duration {
	if m.TickInterval.Duration == 0 {
		return 10 * time.Second
	} else {
		return m.TickInterval.Duration
	}
}

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalText(text []byte) error {
	var err error
	d.Duration, err = time.ParseDuration(string(text))
	return err
}

type Tier struct {
	Name    string
	Targets []string
	Hash    *consistent.Consistent
	Shadows map[string]string
}

var (
	listenCounts = expvar.NewMap("coco.listen")
	filterCounts = expvar.NewMap("coco.filter")
	sendCounts   = expvar.NewMap("coco.send")
	metricCounts = expvar.NewMap("coco.hash.metrics")
	hostCounts   = expvar.NewMap("coco.hash.hosts")
	ratioCounts  = expvar.NewMap("coco.hash.ratios")
	lookupCounts = expvar.NewMap("coco.lookup")
	queueCounts  = expvar.NewMap("coco.queues")
	errorCounts  = expvar.NewMap("coco.errors")
)
