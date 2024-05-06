package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	serverEnvKey              = "EXT_SERVER_HOST"
	portsEnvKey                = "EXT_SERVER_PORTS"
	egressIPsEnvKey           = "EGRESS_IPS"
	hostSubnetEnvKey           = "HOST_SUBNET"
	delayBetweenRequestEnvKey = "DELAY_BETWEEN_REQ_SEC"
	reqTimeoutEnvKey          = "REQ_TIMEOUT_SEC"
	envKeyErrMsg              = "define env key %q"
	defaultDelayBetweenReqSec = 1
	defaultRequestTimeoutSec  = 1
)

func main() {
	wg := &sync.WaitGroup{}
	stop := registerSignalHandler()
	extHost, extPorts, egressIPsStr, hostSubnetStr, delayBetweenReq, timeout := processEnvVars()
	egressIPs := make(map[string]struct{})
	if egressIPsStr != "" {
		egressIPs = buildEIPMap(egressIPsStr)
	}
	startupNonEIPTick, eipStartUpLatency, eipRecoveryLatency, eipTick, nonEIPTick, failure := buildAndRegisterMetrics(delayBetweenReq)
	wg.Add(2)
	startMetricsServer(stop, wg)
	// begin requests until Egress IP found
	wg.Add(1)
	go checkEIPAndNonEIPUntilStop(stop, wg, egressIPs, hostSubnetStr, extHost, extPorts, eipStartUpLatency, eipRecoveryLatency, startupNonEIPTick, eipTick, nonEIPTick, failure, delayBetweenReq, timeout)
	wg.Wait()
}

// validate hostip or eip
func validateIPAddress(ipAddr string, egressIPs map[string]struct{}, subnet string) bool {
    if len(egressIPs) > 0 {
	if _, ok := egressIPs[ipAddr]; ok {
	    return true
	}
    } else {
	ip := net.ParseIP(ipAddr)
	if ip == nil {
	    log.Printf("Error:  IP Address is nil")
	    return false
	}
	// Parse the subnet
	_, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
	    log.Printf("Error:  Failed to parse subnet: %v", err)
            return false
	}
        // Check if the IP address is within the subnet
	return ipNet.Contains(ip)
    }
    return false
}


func checkEIPAndNonEIPUntilStop(stop <-chan struct{}, wg *sync.WaitGroup, egressIPs map[string]struct{}, hostSubnetStr string, extHost, extPorts string,
        eipStartUpLatency, eipRecoveryLatency *prometheus.Gauge, startupNonEIPTick, eipTick, nonEIPTick *prometheus.Gauge, failure *prometheus.Gauge, delayBetweenReq, timeout int) {
	log.Print("## checkEIPAndNonEIPUntilStop: Polling source IP and increment metric counts for when Egress IP or another IP seen as source IP")
	defer wg.Done()
	var done bool
	start := time.Now()
	var eipCheckFailed bool
	var startupLatencySet bool
	var err error
	var req *http.Request
	var res *http.Response
	var valid bool

	for !done {
		select {
		case <-stop:
			done = true
		default:
			err = nil
			client := getHTTPClient(timeout)
			// Create a new request
			url := buildDstURL(extHost, extPorts)
			req, err = http.NewRequest("GET", url, nil)
			if err != nil {
				log.Printf("Error: %v , while calling http.NewRequest", err)
			} else {
				// Set Close to true to indicate that the connection should be closed after sending the request
				res, err = client.Do(req)
				if err != nil {
					log.Printf("Error: %v , while calling client.Do", err)
				} else {
					if res.StatusCode == http.StatusOK {
						resBody, err := ioutil.ReadAll(res.Body)
						if err != nil {
							log.Printf("Error: %v , while calling ioutil.ReadAll", err)
						} else {
							valid = validateIPAddress(string(resBody), egressIPs, hostSubnetStr)
						}
					} else {
						log.Printf("res.StatusCode %d", res.StatusCode)
						err = errors.New(fmt.Sprintf("res.StatusCode %d", res.StatusCode))
					}
					res.Body.Close()
				}
			}
			// Close idle connections after requests are done
			client.CloseIdleConnections()
			if err != nil {
				if eipCheckFailed == false {
					eipCheckFailed = true
					start = time.Now()
				}
				(*failure).Inc()
			} else {
				if valid {
					if startupLatencySet == false {
						(*eipStartUpLatency).Set(time.Now().Sub(start).Seconds())
						log.Printf("Startup Latency %v", time.Now().Sub(start).Seconds())
						startupLatencySet = true
					} else {
						if eipCheckFailed == true {
							eipCheckFailed = false
							(*eipRecoveryLatency).Set(time.Now().Sub(start).Seconds())
							log.Printf("Failover Latency %v", time.Now().Sub(start).Seconds())
							start = time.Now()
						}
					}
				} else {
					if startupLatencySet == false {
						(*startupNonEIPTick).Inc()
					} else {
						if eipCheckFailed == false {
							eipCheckFailed = true
							start = time.Now()
						}
						(*nonEIPTick).Inc()
					}
				}
			}
			res = nil
			req = nil
			if delayBetweenReq != 0 {
				time.Sleep(time.Duration(delayBetweenReq) * time.Second)
			}
		}
	}
	log.Print("Finished polling source IP")
}

func isIP(s string) bool {
	return net.ParseIP(s) != nil
}

func buildDstURL(host, portRange string) string {
	return fmt.Sprintf("http://%s:%s", host, portRange)
	// Parse the port range string
	ports := strings.Split(portRange, ":")
	startPort, _ := strconv.Atoi(ports[0])
	endPort, _ := strconv.Atoi(ports[1])

	// Seed the random number generator
	rand.Seed(time.Now().UnixNano())

	// Generate a random port within the specified range
	randomPort := startPort + rand.Intn(endPort-startPort+1)

	// Construct the URL with the randomly selected port
	return fmt.Sprintf("http://%s:%d", host, randomPort)
}

func getHTTPClient(timeout int) http.Client {
	return http.Client{
		Timeout: time.Duration(timeout) * time.Second,
	}
}

func buildEIPMap(egressIPsStr string) map[string]struct{} {
	// build map of egress IPs
	egressIPs := strings.Split(egressIPsStr, ",")
	egressIPMap := make(map[string]struct{})
	for _, egressIP := range egressIPs {
		if ip := net.ParseIP(egressIP); ip == nil {
			panic(fmt.Sprintf("invalid egress IPs - comma seperated list allowed: %q", egressIPsStr))
		}
		egressIPMap[egressIP] = struct{}{}
	}
	return egressIPMap
}

func processEnvVars() (string, string, string, string, int, int) {
	var err error
	extHost := os.Getenv(serverEnvKey)
	if extHost == "" {
		panic(fmt.Sprintf(envKeyErrMsg, serverEnvKey))
	}
	extPorts := os.Getenv(portsEnvKey)
	if extPorts == "" {
		panic(fmt.Sprintf(envKeyErrMsg, portsEnvKey))
	}
	hostSubnetStr := ""
	egressIPsStr := os.Getenv(egressIPsEnvKey)
	if egressIPsStr == "" {
		hostSubnetStr = os.Getenv(hostSubnetEnvKey)
		if hostSubnetStr == "" {
			panic(fmt.Sprintf(envKeyErrMsg, egressIPsEnvKey))
		}
	}

	delayBetweenReq := defaultDelayBetweenReqSec
	delayBetweenRequestStr := os.Getenv(delayBetweenRequestEnvKey)
	if delayBetweenRequestStr != "" {
		delayBetweenReq, err = strconv.Atoi(delayBetweenRequestStr)
		if err != nil {
			panic(fmt.Sprintf("failed to parse delay between requests: %v", err))
		}
	}
	requestTimeout := defaultRequestTimeoutSec
	reqTimeoutStr := os.Getenv(reqTimeoutEnvKey)
	if reqTimeoutStr != "" {
		requestTimeout, err = strconv.Atoi(reqTimeoutStr)
		if err != nil {
			panic(fmt.Sprintf("failed to parse request timeout %q: %v", reqTimeoutStr, err))
		}
	}
	return extHost, extPorts, egressIPsStr, hostSubnetStr, delayBetweenReq, requestTimeout
}

func registerSignalHandler() chan struct{} {
	stop := make(chan struct{})
	c := make(chan os.Signal)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-c
		close(stop)
	}()
	return stop
}

func startMetricsServer(stop <-chan struct{}, wg *sync.WaitGroup) {
	// build metrics server
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	server := &http.Server{Addr: ":8080", Handler: mux}
	// start metrics server
	go func() {
		defer wg.Done()
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			panic(err.Error())
		}
	}()
	// stop server when done triggered
	go func() {
		defer wg.Done()
		<-stop
		if err := server.Close(); err != nil {
			panic(err.Error())
		}
	}()
}

func buildAndRegisterMetrics(delayBetweenReq int) (*prometheus.Gauge, *prometheus.Gauge, *prometheus.Gauge, *prometheus.Gauge, *prometheus.Gauge, *prometheus.Gauge) {
	var startupNonEIPTick = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "scale",
		Name:      "startup_non_eip_total",
		Help:      fmt.Sprintf("during startup, increments every time EgressIP not seen as source IP - increments every %d seconds if seen", delayBetweenReq),
	})

	var eipStartUpLatency = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "scale",
		Name:      "eip_startup_latency_total",
		Help: fmt.Sprintf("time it takes in seconds for a connection to have a source IP of EgressIP at startup"+
			" with polling interval of %d seconds", delayBetweenReq),
	})
	var eipRecoveryLatency = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "scale",
		Name:      "eip_recovery_latency",
		Help: fmt.Sprintf("time it takes in seconds for an Egress IP connection to recover from failure"+
			" with polling interval of %d seconds", delayBetweenReq),
	})

	var eipTick = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "scale",
		Name:      "eip_total",
		Help:      fmt.Sprintf("increments every time EgressIP seen as source IP - increments every %d seconds if seen", delayBetweenReq),
	})

	var nonEIPTick = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "scale",
		Name:      "non_eip_total",
		Help:      fmt.Sprintf("increments every time EgressIP not seen as source IP - increments every %d seconds if seen", delayBetweenReq),
	})

	var failure = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "scale",
		Name:      "failure_total",
		Help:      fmt.Sprintf("increments every time there is a connection failure - increments every %d seconds if seen", delayBetweenReq),
	})
	// create metrics registry and register metrics
	prometheus.MustRegister(startupNonEIPTick)
	prometheus.MustRegister(eipStartUpLatency)
	prometheus.MustRegister(eipRecoveryLatency)
	prometheus.MustRegister(eipTick)
	prometheus.MustRegister(nonEIPTick)
	prometheus.MustRegister(failure)
	return &startupNonEIPTick, &eipStartUpLatency, &eipRecoveryLatency, &eipTick, &nonEIPTick, &failure
}
