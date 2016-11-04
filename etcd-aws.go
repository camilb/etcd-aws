package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/camilb/ec2cluster"
	"github.com/coreos/go-etcd/etcd"
	"github.com/crewjam/awsregion"
)

type etcdState struct {
	Name       string         `json:"name"`
	ID         string         `json:"id"`
	State      string         `json:"state"`
	StartTime  time.Time      `json:"startTime"`
	LeaderInfo etcdLeaderInfo `json:"leaderInfo"`
}

type etcdLeaderInfo struct {
	Leader               string    `json:"leader"`
	Uptime               string    `json:"uptime"`
	StartTime            time.Time `json:"startTime"`
	RecvAppendRequestCnt int       `json:"recvAppendRequestCnt"`
	RecvPkgRate          int       `json:"recvPkgRate"`
	RecvBandwidthRate    int       `json:"recvBandwidthRate"`
	SendAppendRequestCnt int       `json:"sendAppendRequestCnt"`
}

type etcdMembers struct {
	Members []etcdMember `json:"members,omitempty"`
}

type etcdMember struct {
	ID         string   `json:"id,omitempty"`
	Name       string   `json:"name,omitempty"`
	PeerURLs   []string `json:"peerURLs,omitempty"`
	ClientURLs []string `json:"clientURLs,omitempty"`
}

var etcdLocalURL string

var (
	certFile = flag.String("cert", "/etc/etcd2/ssl/etcd-client.pem", "A PEM eoncoded certificate file.")
	keyFile  = flag.String("key", "/etc/etcd2/ssl/etcd-client-key.pem", "A PEM encoded private key file.")
	caFile   = flag.String("CA", "/etc/etcd2/ssl/ca.pem", "A PEM eoncoded CA's certificate file.")
)

func buildCluster(s *ec2cluster.Cluster) (initialClusterState string, initialCluster []string, err error) {
	localInstance, err := s.Instance()
	if err != nil {
		return "", nil, err
	}

	clusterInstances, err := s.Members()
	if err != nil {
		return "", nil, fmt.Errorf("list members: %s", err)
	}

	initialClusterState = "new"
	initialCluster = []string{}
	for _, instance := range clusterInstances {
		if instance.PrivateDnsName == nil {
			continue
		}

		// add this instance to the initialCluster expression
		initialCluster = append(initialCluster, fmt.Sprintf("%s=https://%s:2380",
			*instance.InstanceId, *instance.PrivateDnsName))

		// skip the local node, since we know it is not running yet
		if *instance.InstanceId == *localInstance.InstanceId {
			continue
		}
		instance, err := s.Instance()
		if err != nil {
			log.Debug(err)
			return "", nil, err
		}

		// Load client cert
		cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			log.Debug(err)
		}

		// Load CA cert
		caCert, err := ioutil.ReadFile(*caFile)
		if err != nil {
			log.Debug(err)
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)

		// Setup HTTPS client
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      caCertPool,
		}
		tlsConfig.BuildNameToCertificate()
		transport := &http.Transport{TLSClientConfig: tlsConfig}
		client := &http.Client{Transport: transport}
		// fetch the state of the node.
		resp, err := client.Get(fmt.Sprintf("https://%s:2379/v2/stats/self", *instance.PrivateDnsName))
		if err != nil {
			log.Printf("%s: https://%s:2379/v2/stats/self: %s", *instance.InstanceId,
				*instance.PrivateDnsName, err)
			continue
		}
		nodeState := etcdState{}
		if err := json.NewDecoder(resp.Body).Decode(&nodeState); err != nil {
			log.Printf("%s: https://%s:2379/v2/stats/self: %s", *instance.InstanceId,
				*instance.PrivateDnsName, err)
			continue
		}

		if nodeState.LeaderInfo.Leader == "" {
			log.Printf("%s: https://%s:2379/v2/stats/self: alive, no leader", *instance.InstanceId,
				*instance.PrivateDnsName)
			continue
		}
		log.Printf("Sarmizegetusa")
		log.Printf("%s: https://%s:2379/v2/stats/self: has leader %s", *instance.InstanceId,
			*instance.PrivateDnsName, nodeState.LeaderInfo.Leader)
		if initialClusterState != "existing" {
			initialClusterState = "existing"

			// inform the know we found about the new node we're about to add so that
			// when etcd starts we can avoid etcd thinking the cluster is out of sync.
			log.Printf("joining cluster via %s", *instance.InstanceId)
			m := etcdMember{
				Name:     *localInstance.InstanceId,
				PeerURLs: []string{fmt.Sprintf("https://%s:2380", *localInstance.PrivateDnsName)},
			}
			body, _ := json.Marshal(m)
			client.Post(fmt.Sprintf("https://%s:2379/v2/members", *instance.PrivateDnsName),
				"application/json", bytes.NewReader(body))
		}
	}
	return initialClusterState, initialCluster, nil
}

func main() {
	instanceID := flag.String("instance", "",
		"The instance ID of the cluster member. If not supplied, then the instance ID is determined from EC2 metadata")
	clusterTagName := flag.String("tag", "aws:autoscaling:groupName",
		"The instance tag that is common to all members of the cluster")

	defaultBackupInterval := 1 * time.Minute
	if d := os.Getenv("ETCD_BACKUP_INTERVAL"); d != "" {
		var err error
		defaultBackupInterval, err = time.ParseDuration(d)
		if err != nil {
			log.Printf("ERROR: %s", err)
		}
	}

	backupInterval := flag.Duration("backup-interval", defaultBackupInterval,
		"How frequently to back up the etcd data to S3")
	backupBucket := flag.String("backup-bucket", os.Getenv("ETCD_BACKUP_BUCKET"),
		"The name of the S3 bucket where tha backup is stored. "+
			"Environment variable: ETCD_BACKUP_BUCKET")
	defaultBackupKey := "/etcd-backup.gz"
	if d := os.Getenv("ETCD_BACKUP_KEY"); d != "" {
		defaultBackupKey = d
	}
	backupKey := flag.String("backup-key", defaultBackupKey,
		"The name of the S3 key where tha backup is stored. "+
			"Environment variable: ETCD_BACKUP_KEY")

	defaultDataDir := "/var/lib/etcd2"
	if d := os.Getenv("ETCD_DATA_DIR"); d != "" {
		defaultDataDir = d
	}
	dataDir := flag.String("data-dir", defaultDataDir,
		"The path to the etcd2 data directory. "+
			"Environment variable: ETCD_DATA_DIR")
	flag.Parse()

	var err error
	if *instanceID == "" {
		*instanceID, err = ec2cluster.DiscoverInstanceID()
		if err != nil {
			log.Printf("ERROR: %s", err)
		}
	}

	awsSession := session.New()
	if region := os.Getenv("AWS_REGION"); region != "" {
		awsSession.Config.WithRegion(region)
	}
	awsregion.GuessRegion(awsSession.Config)

	s := &ec2cluster.Cluster{
		AwsSession: awsSession,
		InstanceID: *instanceID,
		TagName:    *clusterTagName,
	}

	localInstance, err := s.Instance()
	if err != nil {
		log.Printf("ERROR: %s", err)
	}

	initialClusterState, initialCluster, err := buildCluster(s)

	// start the backup and restore goroutine.
	shouldTryRestore := false
	if initialClusterState == "new" {
		_, err := os.Stat(filepath.Join(*dataDir, "member"))
		if os.IsNotExist(err) {
			shouldTryRestore = true
		} else {
			log.Printf("%s: %s", filepath.Join(*dataDir, "member"), err)
		}
	}
	go func() {
		// wait for etcd to start
		for {
			etcdClient, err := etcd.NewTLSClient([]string{fmt.Sprintf("https://%s:2379", *localInstance.PrivateDnsName)}, etcdCert, etcdKey, etcdcaCert)
			if err != nil {
				log.Printf("ERROR: %s", err)
			}
			if success := etcdClient.SyncCluster(); success {
				break
			}
			time.Sleep(time.Second)
		}

		if shouldTryRestore {
			if err := restoreBackup(s, *backupBucket, *backupKey, *dataDir); err != nil {
				log.Printf("ERROR: %s", err)
			}
		}

		if err := backupService(s, *backupBucket, *backupKey, *dataDir, *backupInterval); err != nil {
			log.Printf("ERROR: %s", err)
		}
	}()

	// watch for lifecycle events and remove nodes from the cluster as they are
	// terminated.
	go watchLifecycleEvents(s, localInstance)

	// Run the etcd command
	cmd := exec.Command("etcd2")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = []string{
		fmt.Sprintf("ETCD_NAME=%s", *localInstance.InstanceId),
		fmt.Sprintf("ETCD_PEER_TRUSTED_CA_FILE=/etc/etcd2/ssl/ca.pem"),
		fmt.Sprintf("ETCD_PEER_CERT_FILE=/etc/etcd2/ssl/etcd.pem"),
		fmt.Sprintf("ETCD_PEER_KEY_FILE=/etc/etcd2/ssl/etcd-key.pem"),
		fmt.Sprintf("ETCD_CLIENT_CERT_AUTH=true"),
		fmt.Sprintf("ETCD_TRUSTED_CA_FILE=/etc/etcd2/ssl/ca.pem"),
		fmt.Sprintf("ETCD_CERT_FILE=/etc/etcd2/ssl/etcd.pem"),
		fmt.Sprintf("ETCD_KEY_FILE=/etc/etcd2/ssl/etcd-key.pem"),
		fmt.Sprintf("ETCD_DATA_DIR=/var/lib/etcd2"),
		fmt.Sprintf("ETCD_ELECTION_TIMEOUT=1500"),
		fmt.Sprintf("ETCD_HEARTBEAT_INTERVAL=300"),
		fmt.Sprintf("ETCD_ADVERTISE_CLIENT_URLS=https://%s:2379", *localInstance.PrivateDnsName),
		fmt.Sprintf("ETCD_LISTEN_CLIENT_URLS=https://0.0.0.0:2379"),
		fmt.Sprintf("ETCD_LISTEN_PEER_URLS=https://0.0.0.0:2380"),
		fmt.Sprintf("ETCD_INITIAL_CLUSTER_STATE=%s", initialClusterState),
		fmt.Sprintf("ETCD_INITIAL_CLUSTER=%s", strings.Join(initialCluster, ",")),
		fmt.Sprintf("ETCD_INITIAL_ADVERTISE_PEER_URLS=https://%s:2380", *localInstance.PrivateDnsName),
		fmt.Sprintf("ETCD_DEBUG=false"),
	}
	asg, _ := s.AutoscalingGroup()
	if asg != nil {
		cmd.Env = append(cmd.Env, fmt.Sprintf("ETCD_INITIAL_CLUSTER_TOKEN=%s", *asg.AutoScalingGroupARN))
	}
	for _, env := range cmd.Env {
		log.Printf("%s", env)
	}
	if err := cmd.Run(); err != nil {
		log.Printf("%s", err)
	}
}
