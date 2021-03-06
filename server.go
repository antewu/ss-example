package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fortuna/ss-example/metrics"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/shadowsocks/go-shadowsocks2/core"
	ssnet "github.com/shadowsocks/go-shadowsocks2/net"
	"github.com/shadowsocks/go-shadowsocks2/shadowaead"
	"github.com/shadowsocks/go-shadowsocks2/socks"
)

var config struct {
	UDPTimeout time.Duration
}

func shadowConn(conn ssnet.DuplexConn, cipherList []shadowaead.Cipher) (ssnet.DuplexConn, int, error) {
	cipher, index, shadowReader, err := findCipher(conn, cipherList)
	if err != nil {
		return nil, -1, err
	}
	shadowWriter := shadowaead.NewShadowsocksWriter(conn, cipher)
	return ssnet.WrapDuplexConn(conn, shadowReader, shadowWriter), index, nil
}

func findCipher(clientReader io.Reader, cipherList []shadowaead.Cipher) (shadowaead.Cipher, int, io.Reader, error) {
	if len(cipherList) == 0 {
		return nil, -1, nil, errors.New("Empty cipher list")
	} else if len(cipherList) == 1 {
		return cipherList[0], 0, shadowaead.NewShadowsocksReader(clientReader, cipherList[0]), nil
	}
	// buffer saves the bytes read from shadowConn, in order to allow for replays.
	var buffer bytes.Buffer
	// Try each cipher until we find one that authenticates successfully.
	// This assumes that all ciphers are AEAD.
	for i, cipher := range cipherList {
		log.Printf("Trying cipher %v", i)
		// tmpReader reuses the bytes read so far, falling back to shadowConn if it needs more
		// bytes. All bytes read from shadowConn are saved in buffer.
		tmpReader := io.MultiReader(bytes.NewReader(buffer.Bytes()), io.TeeReader(clientReader, &buffer))
		// Override the Reader of shadowConn so we can reset it for each cipher test.
		cipherReader := shadowaead.NewShadowsocksReader(tmpReader, cipher)
		// Read should read just enough data to authenticate the payload size.
		_, err := cipherReader.Read(make([]byte, 0))
		if err != nil {
			log.Printf("Failed cipher %v: %v", i, err)
			continue
		}
		log.Printf("Selected cipher %v", i)
		// We don't need to replay the bytes anymore, but we don't want to drop those
		// read so far.
		return cipher, i, shadowaead.NewShadowsocksReader(io.MultiReader(&buffer, clientReader), cipher), nil
	}
	return nil, -1, nil, fmt.Errorf("could not find valid cipher")
}

func getNetKey(addr net.Addr) (string, error) {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", errors.New("Failed to parse ip")
	}
	ipNet := net.IPNet{IP: ip}
	if ip.To4() != nil {
		ipNet.Mask = net.CIDRMask(24, 32)
	} else {
		ipNet.Mask = net.CIDRMask(32, 128)
	}
	return ipNet.String(), nil
}

// Listen on addr for incoming connections.
func tcpRemote(addr string, cipherList []shadowaead.Cipher, m metrics.TCPMetrics) {
	accessKeyMetrics := metrics.NewMetricsMap()
	netMetrics := metrics.NewMetricsMap()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("failed to listen on %s: %v", addr, err)
		return
	}

	log.Printf("listening TCP on %s", addr)
	for {
		var clientConn ssnet.DuplexConn
		clientConn, err := l.(*net.TCPListener).AcceptTCP()
		m.AddTCPConnection()
		if err != nil {
			log.Printf("failed to accept: %v", err)
			return
		}

		go func() {
			defer clientConn.Close()
			connStart := time.Now()
			clientConn.(*net.TCPConn).SetKeepAlive(true)
			accessKey := "INVALID"
			// TODO: create status enums and move to metrics.go
			status := "OK"
			netKey, err := getNetKey(clientConn.RemoteAddr())
			if err != nil {
				netKey = "INVALID"
			}
			var proxyMetrics metrics.ProxyMetrics
			defer func() {
				connEnd := time.Now()
				connDuration := connEnd.Sub(connStart)
				log.Printf("Done with status %v, duration %v", status, connDuration)
				m.RemoveTCPConnection(accessKey, status, connDuration)
				accessKeyMetrics.Add(accessKey, proxyMetrics)
				log.Printf("Key %v: %s", accessKey, metrics.SPrintMetrics(accessKeyMetrics.Get(accessKey)))
				netMetrics.Add(netKey, proxyMetrics)
				log.Printf("Net %v: %s", netKey, metrics.SPrintMetrics(netMetrics.Get(netKey)))
			}()

			clientConn = metrics.MeasureConn(clientConn, &proxyMetrics.ProxyClient, &proxyMetrics.ClientProxy)
			clientConn, index, err := shadowConn(clientConn, cipherList)
			if err != nil {
				log.Printf("Failed to find a valid cipher: %v", err)
				status = "ERR_CIPHER"
				return
			}
			accessKey = strconv.Itoa(index)

			tgt, err := socks.ReadAddr(clientConn)
			if err != nil {
				log.Printf("failed to get target address: %v", err)
				status = "ERR_READ_ADDRESS"
				return
			}

			c, err := net.Dial("tcp", tgt.String())
			if err != nil {
				log.Printf("failed to connect to target: %v", err)
				status = "ERR_CONNECT"
				return
			}
			var tgtConn ssnet.DuplexConn = c.(*net.TCPConn)
			defer tgtConn.Close()
			tgtConn.(*net.TCPConn).SetKeepAlive(true)
			tgtConn = metrics.MeasureConn(tgtConn, &proxyMetrics.ProxyTarget, &proxyMetrics.TargetProxy)

			log.Printf("proxy %s <-> %s", clientConn.RemoteAddr(), tgt)
			_, _, err = ssnet.Relay(clientConn, tgtConn)
			if err != nil {
				log.Printf("relay error: %v", err)
				status = "ERR_RELAY"
			}
		}()
	}
}

type cipherList []shadowaead.Cipher

func main() {

	var flags struct {
		Server      string
		Ciphers     cipherList
		MetricsAddr string
	}

	flag.StringVar(&flags.Server, "s", "", "server listen address")
	flag.Var(&flags.Ciphers, "u", "available ciphers: "+strings.Join(core.ListCipher(), " "))
	flag.DurationVar(&config.UDPTimeout, "udptimeout", 5*time.Minute, "UDP tunnel timeout")
	flag.StringVar(&flags.MetricsAddr, "metrics", "", "address for the Prometheus metrics")
	flag.Parse()

	if flags.Server == "" || len(flags.Ciphers) == 0 {
		flag.Usage()
		return
	}

	if flags.MetricsAddr != "" {
		http.Handle("/metrics", promhttp.Handler())
		go func() {
			log.Fatal(http.ListenAndServe(flags.MetricsAddr, nil))
		}()
		log.Printf("Metrics on http://%v/metrics", flags.MetricsAddr)
	}

	go udpRemote(flags.Server, flags.Ciphers)
	go tcpRemote(flags.Server, flags.Ciphers, metrics.NewPrometheusTCPMetrics())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
}

func (sl *cipherList) Set(flagValue string) error {
	e := strings.SplitN(flagValue, ":", 2)
	if len(e) != 2 {
		return fmt.Errorf("Missing colon")
	}
	cipher, err := core.PickCipher(e[0], nil, e[1])
	if err != nil {
		return err
	}
	aead, ok := cipher.(shadowaead.Cipher)
	if !ok {
		log.Fatal("Only AEAD ciphers are supported")
	}
	*sl = append(*sl, aead)
	return nil
}

func (sl *cipherList) String() string {
	return fmt.Sprint(*sl)
}
