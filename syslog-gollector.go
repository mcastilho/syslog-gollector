package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/otoolep/syslog-gollector/input"
	"github.com/otoolep/syslog-gollector/output"

	log "code.google.com/p/log4go"
)

// Program parameters
var adminIface string
var tcpIface string
var udpIface string
var kBrokers string
var kBatch int
var kTopic string
var kBufferTime int
var kBufferBytes int
var pEnabled bool
var cCapacity int
var noReport bool

// Program resources
var tcpServer *input.TcpServer
var udpServer *input.UdpServer
var parser *input.Rfc5424Parser

// Diagnostic data
var startTime time.Time

// Types
const (
	adminHost        = "localhost:8080"
	connTcpHost      = "localhost:514"
	connUdpHost      = "localhost:514"
	connType         = "tcp"
	kafkaBatch       = 10
	kafkaBrokers     = "localhost:9092"
	kafkaTopic       = "logs"
	kafkaBufferTime  = 1000
	kafkaBufferBytes = 512 * 1024
	parseEnabled     = true
	chanCapacity     = 0
)

func init() {
	flag.StringVar(&adminIface, "admin", adminHost, "Admin interface")
	flag.StringVar(&tcpIface, "tcp", connTcpHost, "TCP bind interface")
	flag.StringVar(&udpIface, "udp", connUdpHost, "UDP interface")
	flag.StringVar(&kBrokers, "broker", kafkaBrokers, "comma-delimited kafka brokers")
	flag.StringVar(&kTopic, "topic", kafkaTopic, "kafka topic")
	flag.IntVar(&kBatch, "batch", kafkaBatch, "Kafka batch size")
	flag.IntVar(&kBufferTime, "maxbuff", kafkaBufferTime, "Kafka client buffer max time (ms)")
	flag.IntVar(&kBufferBytes, "maxbytes", kafkaBufferBytes, "Kafka client buffer max bytes")
	flag.BoolVar(&pEnabled, "parse", parseEnabled, "enable syslog header parsing")
	flag.IntVar(&cCapacity, "chancap", chanCapacity, "channel buffering capacity")
	flag.BoolVar(&noReport, "noreport", false, "disable anonymized launch reporting")
}

// isPretty returns whether the HTTP response body should be pretty-printed.
func isPretty(req *http.Request) (bool, error) {
	err := req.ParseForm()
	if err != nil {
		return false, err
	}
	if _, ok := req.Form["pretty"]; ok {
		return true, nil
	}
	return false, nil
}

// ServeStatistics returns the statistics for the program
func ServeStatistics(w http.ResponseWriter, req *http.Request) {
	statistics := make(map[string]interface{})
	resources := map[string]input.Statistics{"tcp": tcpServer, "udp": udpServer, "parser": parser}
	for k, v := range resources {
		s, err := v.GetStatistics()
		if err != nil {
			log.Error("failed to get " + k + " stats")
			http.Error(w, "failed to get "+k+" stats", http.StatusInternalServerError)
			return
		}
		statistics[k] = s
	}

	var b []byte
	var err error
	pretty, _ := isPretty(req)
	if pretty {
		b, err = json.MarshalIndent(statistics, "", "    ")
	} else {
		b, err = json.Marshal(statistics)
	}
	if err != nil {
		log.Error("failed to JSON marshal statistics map")
		http.Error(w, "failed to JSON marshal statistics map", http.StatusInternalServerError)
		return
	}
	w.Write(b)
}

func ServeDiagnostics(w http.ResponseWriter, req *http.Request) {
	diagnostics := make(map[string]string)
	diagnostics["started"] = startTime.String()
	diagnostics["uptime"] = time.Since(startTime).String()
	diagnostics["kafkaBatch"] = strconv.Itoa(kafkaBatch)
	diagnostics["kBufferTime"] = strconv.Itoa(kBufferTime)
	diagnostics["kBufferBytes"] = strconv.Itoa(kBufferBytes)
	diagnostics["cCapacity"] = strconv.Itoa(cCapacity)
	diagnostics["kTopic"] = kTopic

	if pEnabled {
		diagnostics["parsing"] = "enabled"
	} else {
		diagnostics["parsing"] = "disabled"
	}

	var b []byte
	pretty, _ := isPretty(req)
	if pretty {
		b, _ = json.MarshalIndent(diagnostics, "", "    ")
	} else {
		b, _ = json.Marshal(diagnostics)
	}
	w.Write(b)
}

func main() {
	flag.Parse()

	startTime = time.Now()

	hostname, err := os.Hostname()
	if err != nil {
		log.Error("unable to determine hostname -- aborting")
		os.Exit(1)
	}
	log.Info("syslog server starting on %s, PID %d", hostname, os.Getpid())
	log.Info("machine has %d cores", runtime.NumCPU())

	// Log config
	log.Info("Admin server: %s", adminIface)
	log.Info("kafka brokers: %s", kBrokers)
	log.Info("kafka topic: %s", kTopic)
	log.Info("kafka batch size: %d", kBatch)
	log.Info("kafka buffer time: %dms", kBufferTime)
	log.Info("kafka buffer bytes: %d", kBufferBytes)
	log.Info("parsing enabled: %t", pEnabled)
	log.Info("channel buffering capacity: %d", cCapacity)

	// Prep the channels
	rawChan := make(chan string, cCapacity)
	prodChan := make(chan string, cCapacity)

	parser = input.NewRfc5424Parser()
	if pEnabled {
		// Feed the input through the Parser stage
		prodChan, err = parser.StreamingParse(rawChan)
	} else {
		// Pass the input directly to the output
		prodChan = rawChan
	}

	// Start the event servers
	tcpServer = input.NewTcpServer(tcpIface)
	err = tcpServer.Start(func() chan<- string {
		return rawChan
	})
	if err != nil {
		fmt.Println("Failed to start TCP server", err.Error())
		os.Exit(1)
	}
	log.Info("listening on %s for TCP connections", tcpIface)

	udpServer = input.NewUdpServer(udpIface)
	err = udpServer.Start(func() chan<- string {
		return rawChan
	})
	if err != nil {
		fmt.Println("Failed to start UDP server", err.Error())
		os.Exit(1)
	}
	log.Info("listening on %s for UDP packets", udpIface)

	// Configure and start the Admin server
	http.HandleFunc("/statistics", ServeStatistics)
	http.HandleFunc("/diagnostics", ServeDiagnostics)
	go func() {
		err = http.ListenAndServe(adminIface, nil)
		if err != nil {
			fmt.Println("Failed to start admin server", err.Error())
			os.Exit(1)
		}
	}()
	log.Info("Admin server started")

	// Connect to Kafka
	log.Info("attempting to connect to Kafka %s...", kBrokers)
	_, err = output.NewKafkaProducer(prodChan, strings.Split(kBrokers, ","), kTopic, kBufferTime, kBufferBytes)
	if err != nil {
		fmt.Println("Failed to create Kafka producer", err.Error())
		os.Exit(1)
	}
	log.Info("connected to Kafka at %s", kBrokers)

	if !noReport {
		reportLaunch()
	}

	// Spin forever
	select {}
}

func reportLaunch() {
	json := fmt.Sprintf(`{"os": "%s", "arch": "%s", "app": "gollector"}`, runtime.GOOS, runtime.GOARCH)
	data := bytes.NewBufferString(json)
	client := http.Client{Timeout: time.Duration(5 * time.Second)}
	go client.Post("https://logs-01.loggly.com/inputs/8a0edd84-92ba-46e4-ada8-c529d0f105af/tag/reporting/",
		"application/json", data)
}
