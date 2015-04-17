package coco

import (
	"log"
	"strings"
	"bytes"
	"regexp"
	"time"
	"github.com/go-martini/martini"
	"encoding/json"
	"encoding/binary"
	"net/http"
	"net"
	"expvar"
	"fmt"
	consistent "github.com/stathat/consistent"
	collectd "github.com/kimor79/gollectd"
)

// Listen for collectd network packets, parse , and send them over a channel
func Listen(config ListenConfig, c chan collectd.Packet) {
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
			errorCounts.Add("listen.receive", 1)
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

func MetricName(packet collectd.Packet) (string) {
	parts := []string{
		packet.Hostname,
		packet.Plugin,
		packet.PluginInstance,
		packet.Type,
		packet.TypeInstance,
	}

	var prts []string

	for _, p := range parts {
		if len(p) != 0 {
			prts = append(prts, p)
		}
	}

	return strings.Join(prts, "/")
}

func Filter(config FilterConfig, raw chan collectd.Packet, filtered chan collectd.Packet, servers map[string]map[string]int64) {
	servers["filtered"] = make(map[string]int64)
	for {
		packet := <- raw
		name := MetricName(packet)

		re := regexp.MustCompile(config.Blacklist)
		if (re.FindStringIndex(name) == nil) {
			filtered <- packet
			filterCounts.Add("accepted", 1)
		} else {
			servers["filtered"][name] = time.Now().Unix()
			filterCounts.Add("rejected", 1)
		}
	}
}

func Send(config map[string]SendConfig, filtered chan collectd.Packet, hashes []*consistent.Consistent, servers map[string]map[string]int64) {
	connections := make(map[string]net.Conn)

	for tier, _ := range config {
		hash := consistent.New()

		targets := config[tier].Targets
		for _, t := range(targets) {
			conn, err := net.Dial("udp", t)
			if err != nil {
				log.Printf("error: send: %s: %+v", t, err)
				errorCounts.Add("send.dial", 1)
			} else {
				// Only add the target to the hash if the connection can initially be established
				re := regexp.MustCompile("^(127.|localhost)")
				if re.FindStringIndex(conn.RemoteAddr().String()) != nil {
					log.Printf("warning: %s is local. You may be looping metrics back to coco!", conn.RemoteAddr())
					log.Printf("warning: dutifully adding %s anyway, but beware!", conn.RemoteAddr())
				}
				servers[t] = make(map[string]int64)
				connections[t] = conn
				hash.Add(t)
				hashCounts.Set(t, &expvar.Int{})
			}
		}

		// Add the hash to the list of hashes
		hashes = append(hashes, hash)
	}

	// Log how the hashes are set up
	i := 0
	for tier, _ := range config {
		hash := hashes[i]
		log.Printf("info: send: tier %s hash ring has %d members: %s", tier, len(hash.Members()), hash.Members())
		i += 1
	}

	if len(connections) == 0 {
		log.Fatal("fatal: no valid write targets in consistent hash ring")
	}

	for {
		packet := <- filtered
		for _, hash := range hashes {
			// Get the server we should forward the packet to
			server, err := hash.Get(packet.Hostname)
			if err != nil {
				log.Fatal(err)
			}
			// Update metadata
			name := MetricName(packet)
			servers[server][name] = time.Now().Unix()

			// Dispatch the metric
			payload := Encode(packet)
			_, err = connections[server].Write(payload)
			if err != nil {
				errorCounts.Add("send.write", 1)
				continue
			}

			// Update counters
			hashCounts.Get(server).(*expvar.Int).Set(int64(len(servers[server])))
			sendCounts.Add(server, 1)
			sendCounts.Add("total", 1)
		}
	}
}

// Encode a Packet into the collectd wire protocol format.
func Encode(packet collectd.Packet) ([]byte) {
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
	buf = append(buf, []byte{0, collectd.ParseValues}...) // type
	buf = append(buf, []byte{0, byte(len(valuesBuf) + 6)}...) // length
	buf = append(buf, []byte{0, byte(len(packet.Values))}...) // number of values
	buf = append(buf, valuesBuf...) // values themselves

	return buf
}

func Api(config ApiConfig, hashes[] *consistent.Consistent, servers map[string]map[string]int64) {
    m := martini.Classic()
	// Endpoint for looking up what storage nodes own metrics for a host
	m.Get("/lookup", func(params martini.Params, req *http.Request) []byte {
		qs := req.URL.Query()
		if len(qs["name"]) > 0 {
			name := qs["name"][0]
			var result []string
			for _, hash := range hashes {
				server, err := hash.Get(name)
				if err != nil {
					errorCounts.Add("api.lookup", 1)
					log.Printf("error: api: %s: %+v\n", name, err)
				}
				result = append(result, server)
			}
			//return []byte(server + "\n")
			return []byte(strings.Join(result, "\n"))
		} else {
			return []byte("")
		}
	})
	// Dump out the list of servers Coco is tracking
    m.Group("/servers", func(r martini.Router) {
        r.Get("", func() []byte {
            data, _ := json.Marshal(servers)
            return data
        })
    })
	// Implement expvars.expvarHandler in Martini.
	m.Get("/debug/vars", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprintf(w, "{")
		first := true
		expvar.Do(func(kv expvar.KeyValue) {
			if !first {
				fmt.Fprintf(w, ",")
			}
			first = false
			fmt.Fprintf(w, "%q: %s", kv.Key, kv.Value)
		})
		fmt.Fprintf(w, "}\n")
	})

	log.Printf("info: binding web server to %s", config.Bind)
	log.Fatal(http.ListenAndServe(config.Bind, m))
}

type CocoConfig struct {
	Listen	ListenConfig
	Filter	FilterConfig
	Send	map[string]SendConfig
	Api		ApiConfig
}

type ListenConfig struct {
	Bind	string
	Typesdb	string
}

type FilterConfig struct {
	Blacklist	string
}

type SendConfig struct {
	Targets	[]string
}

type ApiConfig struct {
	Bind	string
}

var (
	listenCounts = expvar.NewMap("listen")
	filterCounts = expvar.NewMap("filter")
	sendCounts = expvar.NewMap("send")
	hashCounts = expvar.NewMap("metrics")
	errorCounts = expvar.NewMap("errors")
)
