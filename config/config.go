package config

import (
	"crypto/rand"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/wvanbergen/kazoo-go"
	"gopkg.in/yaml.v2"
)

// App defines Kafka-Pixy application configuration. It mirrors the structure
// of the JSON configuration file.
type App struct {
	// TCP address that gRPC API server should listen on.
	GRPCAddr string `yaml:"grpc_addr"`

	// TCP address that HTTP API server should listen on.
	TCPAddr string `yaml:"tcp_addr"`

	// Unix domain socket address that HTTP API server should listen on.
	// Listening on a unix domain socket is disabled by default.
	UnixAddr string `yaml:"unix_addr"`

	// An arbitrary number of proxies to different Kafka/ZooKeeper clusters can
	// be configured.
	Proxies map[string]*Proxy `yaml:"proxies"`

	// Default proxy is the one to be used in API calls that do not start with
	// prefix `/proxy/<alias>`. If it is not explicitly provided, then the one
	// mentioned in the `Proxies` section first is assumed.
	DefaultProxy string `yaml:"default_proxy"`
}

// Proxy defines configuration of a proxy to a particular Kafka/ZooKeeper
// cluster.
type Proxy struct {
	// Unique ID that identifies a Kafka-Pixy instance in both ZooKeeper and
	// Kafka. It is automatically generated by default and it is recommended to
	// leave it like that.
	ClientID string `yaml:"client_id"`

	Kafka struct {

		// List of seed Kafka peers that Kafka-Pixy should access to resolve
		// the Kafka cluster topology.
		SeedPeers []string `yaml:"seed_peers"`
	} `yaml:"kafka"`

	ZooKeeper struct {

		// List of seed ZooKeeper peers that Kafka-Pixy should access to
		// resolve the ZooKeeper cluster topology.
		SeedPeers []string `yaml:"seed_peers"`

		// Path to the directory where Kafka keeps its data.
		Chroot string `yaml:"chroot"`
	} `yaml:"zoo_keeper"`

	Producer struct {

		// Size of all buffered channels created by the producer module.
		ChannelBufferSize int `yaml:"channel_buffer_size"`

		// Period of time that Kafka-Pixy should keep trying to submit buffered
		// messages to Kafka. It is recommended to make it large enough to survive
		// a ZooKeeper leader election in your setup.
		ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	} `yaml:"producer"`

	Consumer struct {

		// Size of all buffered channels created by the consumer module.
		ChannelBufferSize int `yaml:"channel_buffer_size"`

		// Consume request will wait at most this long until a message from the
		// specified group/topic becomes available.
		LongPollingTimeout time.Duration `yaml:"long_polling_timeout"`

		// Period of time that Kafka-Pixy should keep registration with a
		// consumer group or subscription for a topic in the absence of
		// requests to the consumer group or topic.
		RegistrationTimeout time.Duration `yaml:"registration_timeout"`

		// Period of time that Kafka-Pixy should wait for an acknowledgement
		// before retrying. It must be less then RegistrationTimeout.
		AckTimeout time.Duration `yaml:"ack_timeout"`

		// If a request to a Kafka-Pixy fails for any reason, then it should
		// wait this long before retrying.
		BackOffTimeout time.Duration `yaml:"backoff_timeout"`

		// Consumer should wait this long after it gets notification that a
		// consumer joined/left its consumer group before starting rebalancing.
		RebalanceDelay time.Duration `yaml:"rebalance_delay"`

		// How frequently to commit offsets to Kafka.
		OffsetsCommitInterval time.Duration `yaml:"offsets_commit_interval"`
	} `yaml:"consumer"`
}

func (p *Proxy) KazooCfg() *kazoo.Config {
	kazooCfg := kazoo.NewConfig()
	kazooCfg.Chroot = p.ZooKeeper.Chroot
	// ZooKeeper documentation says following about the session timeout: "The
	// current (ZooKeeper) implementation requires that the timeout be a
	// minimum of 2 times the tickTime (as set in the server configuration) and
	// a maximum of 20 times the tickTime". The default tickTime is 2 seconds.
	// See http://zookeeper.apache.org/doc/trunk/zookeeperProgrammers.html#ch_zkSessions
	kazooCfg.Timeout = 15 * time.Second
	return kazooCfg
}

// DefaultApp returns default application configuration where default proxy has
// the specified alias.
func DefaultApp(alias string) *App {
	appCfg := newApp()
	proxyCfg := DefaultProxy()
	appCfg.Proxies[alias] = proxyCfg
	appCfg.DefaultProxy = alias
	return appCfg
}

// DefaultProxy returns configuration used by default.
func DefaultProxy() *Proxy {
	return defaultProxyWithClientID(newClientID())
}

// FromYAML parses configuration from a YAML file and performs basic
// validation of parameters.
func FromYAMLFile(filename string) (*App, error) {
	configFile, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer configFile.Close()
	data, err := ioutil.ReadAll(configFile)
	if err != nil {
		return nil, err
	}

	appCfg, err := FromYAML(data)
	if err != nil {
		return nil, err
	}
	return appCfg, nil
}

// FromYAML parses configuration from a YAML string and performs basic
// validation of parameters.
func FromYAML(data []byte) (*App, error) {
	var prob proxyProb
	if err := yaml.Unmarshal(data, &prob); err != nil {
		return nil, errors.Wrap(err, "failed to parse config")
	}

	appCfg := newApp()
	clientID := newClientID()

	for _, proxyItem := range prob.Proxies {
		proxyAlias, ok := proxyItem.Key.(string)
		if !ok {
			return nil, errors.Errorf("invalid cluster alias, %v", proxyAlias)
		}
		// A hack with marshaling and unmarshaled of a Proxy structure is used
		// here to preserve default values. If we try to unmarshal entire App
		// config, then proxy structures are overridden with zero Proxy values.
		encodedProxyCfg, err := yaml.Marshal(proxyItem.Value)
		if err != nil {
			panic(err)
		}
		proxyCfg := defaultProxyWithClientID(clientID)
		if err := yaml.Unmarshal(encodedProxyCfg, proxyCfg); err != nil {
			return nil, errors.Wrapf(err, "failed to parse proxy config, alias=%s", proxyAlias)
		}
		appCfg.Proxies[proxyAlias] = proxyCfg
		if appCfg.DefaultProxy == "" {
			appCfg.DefaultProxy = proxyAlias
		}
	}

	if err := appCfg.validate(); err != nil {
		return nil, errors.Wrap(err, "invalid config parameter")
	}
	return appCfg, nil
}

func (a *App) validate() error {
	if len(a.Proxies) == 0 {
		return errors.New("at least on proxy must be configured")
	}
	for proxyAlias, proxyCfg := range a.Proxies {
		if err := proxyCfg.validate(); err != nil {
			return errors.Wrapf(err, "invalid config, proxy=%s", proxyAlias)
		}
	}
	return nil
}

func (p *Proxy) validate() error {
	// Validate the Producer parameters.
	switch {
	case p.Producer.ChannelBufferSize <= 0:
		return errors.New("Producer.ChannelBufferSize must be > 0")
	case p.Producer.ShutdownTimeout < 0:
		return errors.New("Producer.ShutdownTimeout must be >= 0")
	}
	// Validate the Consumer parameters.
	switch {
	case p.Consumer.ChannelBufferSize <= 0:
		return errors.New("Consumer.ChannelBufferSize must be > 0")
	case p.Consumer.LongPollingTimeout <= 0:
		return errors.New("Consumer.LongPollingTimeout must be > 0")
	case p.Consumer.RegistrationTimeout <= 0:
		return errors.New("Consumer.RegistrationTimeout must be > 0")
	case p.Consumer.AckTimeout >= p.Consumer.RegistrationTimeout:
		return errors.New("Consumer.AckTimeout must be < Consumer.RegistrationTimeout")
	case p.Consumer.BackOffTimeout <= 0:
		return errors.New("Consumer.BackOffTimeout must be > 0")
	case p.Consumer.RebalanceDelay <= 0:
		return errors.New("Consumer.RebalanceDelay must be > 0")
	case p.Consumer.OffsetsCommitInterval <= 0:
		return errors.New("Consumer.OffsetsCommitInterval must be > 0")
	}
	return nil
}

func newApp() *App {
	appCfg := &App{}
	appCfg.GRPCAddr = "0.0.0.0:19091"
	appCfg.TCPAddr = "0.0.0.0:19092"
	appCfg.Proxies = make(map[string]*Proxy)
	return appCfg
}

func defaultProxyWithClientID(clientID string) *Proxy {
	c := &Proxy{}
	c.ClientID = clientID
	c.ZooKeeper.SeedPeers = []string{"localhost:2181"}
	c.Kafka.SeedPeers = []string{"localhost:9092"}

	c.Producer.ChannelBufferSize = 4096
	c.Producer.ShutdownTimeout = 30 * time.Second

	c.Consumer.ChannelBufferSize = 64
	c.Consumer.LongPollingTimeout = 3 * time.Second
	c.Consumer.RegistrationTimeout = 20 * time.Second
	c.Consumer.AckTimeout = 15 * time.Second
	c.Consumer.BackOffTimeout = 500 * time.Millisecond
	c.Consumer.RebalanceDelay = 250 * time.Millisecond
	c.Consumer.OffsetsCommitInterval = 500 * time.Millisecond
	return c
}

// newClientID creates a unique id that identifies this particular Kafka-Pixy
// in both Kafka and ZooKeeper.
func newClientID() string {
	hostname, err := os.Hostname()
	if err != nil {
		ip, err := getIP()
		if err != nil {
			buffer := make([]byte, 8)
			_, _ = rand.Read(buffer)
			hostname = fmt.Sprintf("%X", buffer)

		} else {
			hostname = ip.String()
		}
	}
	timestamp := time.Now().UTC().Format(time.RFC3339)
	// sarama validation regexp for the client ID doesn't allow ':' characters
	timestamp = strings.Replace(timestamp, ":", ".", -1)
	return fmt.Sprintf("pixy_%s_%s_%d", hostname, timestamp, os.Getpid())
}

func getIP() (net.IP, error) {
	interfaceAddrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var ipv6 net.IP
	for _, interfaceAddr := range interfaceAddrs {
		if ipAddr, ok := interfaceAddr.(*net.IPNet); ok && !ipAddr.IP.IsLoopback() {
			ipv4 := ipAddr.IP.To4()
			if ipv4 != nil {
				return ipv4, nil
			}
			ipv6 = ipAddr.IP
		}
	}
	if ipv6 != nil {
		return ipv6, nil
	}
	return nil, errors.New("Unknown IP address")
}

type proxyProb struct {
	Proxies yaml.MapSlice
}
