package utils

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"regexp"

	log "github.com/Sirupsen/logrus"

	"strings"

	"github.com/containernetworking/cni/pkg/ipam"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/golang/glog"
	"github.com/tigera/libcalico-go/lib/api"
	"github.com/tigera/libcalico-go/lib/client"
	cnet "github.com/tigera/libcalico-go/lib/net"
)

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ValidateNetworkName checks that the network name meets felix's expectations
func ValidateNetworkName(name string) error {
	matched, err := regexp.MatchString(`^[a-zA-Z0-9_\.\-]+$`, name)
	if err != nil {
		return err
	}
	if !matched {
		return errors.New("Invalid characters detected in the given network name. " +
			"Only letters a-z, numbers 0-9, and symbols _.- are supported.")
	}
	return nil
}

// AddIgnoreUnknownArgs appends the 'IgnoreUnknown=1' option to CNI_ARGS before calling the IPAM plugin. Otherwise, it will
// complain about the Kubernetes arguments. See https://github.com/kubernetes/kubernetes/pull/24983
func AddIgnoreUnknownArgs() error {
	cniArgs := "IgnoreUnknown=1"
	if os.Getenv("CNI_ARGS") != "" {
		cniArgs = fmt.Sprintf("%s;%s", cniArgs, os.Getenv("CNI_ARGS"))
	}
	return os.Setenv("CNI_ARGS", cniArgs)
}

func CreateResultFromEndpoint(ep *api.WorkloadEndpoint) (*types.Result, error) {
	result := &types.Result{}

	for _, v := range ep.Spec.IPNetworks {
		unparsedIP := fmt.Sprintf(`{"ip": "%s"}`, v.String())
		parsedIP := types.IPConfig{}
		if err := parsedIP.UnmarshalJSON([]byte(unparsedIP)); err != nil {
			log.Errorf("Error unmarshalling existing endpoint IP: %s", err)
			return nil, err
		}

		if len(v.IP) == net.IPv4len {
			result.IP4 = &parsedIP
		} else {
			result.IP6 = &parsedIP
		}
	}

	return result, nil
}

func GetIdentifiers(args *skel.CmdArgs) (workloadID string, orchestratorID string, err error) {
	// Determine if running under k8s by checking the CNI args
	k8sArgs := K8sArgs{}
	if err = types.LoadArgs(args.Args, &k8sArgs); err != nil {
		return workloadID, orchestratorID, err
	}

	if string(k8sArgs.K8S_POD_NAMESPACE) != "" && string(k8sArgs.K8S_POD_NAME) != "" {
		workloadID = fmt.Sprintf("%s.%s", k8sArgs.K8S_POD_NAMESPACE, k8sArgs.K8S_POD_NAME)
		orchestratorID = "k8s"
	} else {
		workloadID = args.ContainerID
		orchestratorID = "cni"
	}
	return workloadID, orchestratorID, nil
}

func PopulateEndpointNets(endpoint *api.WorkloadEndpoint, result *types.Result) error {
	if result.IP4 == nil && result.IP6 == nil {
		return errors.New("IPAM plugin did not return any IP addresses")
	}

	if result.IP4 != nil {
		result.IP4.IP.Mask = net.CIDRMask(32, 32)
		endpoint.Spec.IPNetworks = append(endpoint.Spec.IPNetworks, cnet.IPNet{result.IP4.IP})
	}

	if result.IP6 != nil {
		result.IP6.IP.Mask = net.CIDRMask(128, 128)
		endpoint.Spec.IPNetworks = append(endpoint.Spec.IPNetworks, cnet.IPNet{result.IP6.IP})
	}

	return nil
}

func CreateClient(conf NetConf) (*client.Client, error) {
	if err := ValidateNetworkName(conf.Name); err != nil {
		return nil, err
	}

	// Use the config file to override environment variables.
	// These variables will be loaded into the client config.
	if conf.EtcdAuthority != "" {
		if err := os.Setenv("ETCD_AUTHORITY", conf.EtcdAuthority); err != nil {
			return nil, err
		}
	}
	if conf.EtcdEndpoints != "" {
		if err := os.Setenv("ETCD_ENDPOINTS", conf.EtcdEndpoints); err != nil {
			return nil, err
		}
	}
	if conf.EtcdScheme != "" {
		if err := os.Setenv("ETCD_SCHEME", conf.EtcdScheme); err != nil {
			return nil, err
		}
	}
	if conf.EtcdKeyFile != "" {
		if err := os.Setenv("ETCD_KEY_FILE", conf.EtcdKeyFile); err != nil {
			return nil, err
		}
	}
	if conf.EtcdCertFile != "" {
		if err := os.Setenv("ETCD_CERT_FILE", conf.EtcdCertFile); err != nil {
			return nil, err
		}
	}
	if conf.EtcdCaCertFile != "" {
		if err := os.Setenv("ETCD_CA_CERT_FILE", conf.EtcdCaCertFile); err != nil {
			return nil, err
		}
	}

	// Load the client config from the current environment.
	clientConfig, err := client.LoadClientConfig("")
	if err != nil {
		return nil, err
	}

	// Create a new client.
	calicoClient, err := client.New(*clientConfig)
	if err != nil {
		return nil, err
	}
	return calicoClient, nil
}

// ReleaseIPAM is called to cleanup IPAM allocations if something goes wrong during
// CNI ADD execution.
func ReleaseIPAllocation(logger *log.Entry, ipamType string, stdinData []byte) {
	logger.Info("Cleaning up IP allocations for failed ADD")
	if err := os.Setenv("CNI_COMMAND", "DEL"); err != nil {
		// Failed to set CNI_COMMAND to DEL.
		logger.Warning("Failed to set CNI_COMMAND=DEL")
	} else {
		if err := ipam.ExecDel(ipamType, stdinData); err != nil {
			// Failed to cleanup the IP allocation.
			logger.Warning("Failed to clean up IP allocations for failed ADD")
		}
	}
}

// Set up logging for both Calico and libcalico usng the provided log level,
func ConfigureLogging(logLevel string) {
	// Debug is _everything_
	// Info is just CNI info level logs
	// Warning is the default - and again is just CNI logs
	if strings.EqualFold(logLevel, "debug") {
		// Enable glogging for libcalico
		_ = flag.Set("logtostderr", "true")
		_ = flag.Set("v", "10")
		_ = flag.Set("stderrthreshold", "10")
		flag.Parse()
		glog.Info("libcalico glog logging configured")

		// Change logging level for CNI plugin
		log.SetLevel(log.DebugLevel)
	} else if strings.EqualFold(logLevel, "info") {
		log.SetLevel(log.InfoLevel)
	} else {
		// Default level
		log.SetLevel(log.WarnLevel)
	}

	log.SetOutput(os.Stderr)
}

// Create a logger which always includes common fields
func CreateContextLogger(workloadID string) *log.Entry {
	// A common pattern is to re-use fields between logging statements by re-using
	// the logrus.Entry returned from WithFields()
	contextLogger := log.WithFields(log.Fields{
		"WorkloadID": workloadID,
	})

	return contextLogger
}
