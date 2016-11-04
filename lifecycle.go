package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/camilb/ec2cluster"
)

// handleLifecycleEvent is invoked whenever we get a lifecycle terminate message. It removes
// terminated instances from the etcd cluster.
func handleLifecycleEvent(m *ec2cluster.LifecycleMessage) (shouldContinue bool, err error) {
	if m.LifecycleTransition != "autoscaling:EC2_INSTANCE_TERMINATING" {
		return true, nil
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

	// look for the instance in the cluster
	resp, err := client.Get(fmt.Sprintf("%s/v2/members", etcdLocalURL))
	if err != nil {
		return false, err
	}
	members := etcdMembers{}
	if err := json.NewDecoder(resp.Body).Decode(&members); err != nil {
		return false, err
	}
	memberID := ""
	for _, member := range members.Members {
		if member.Name == m.EC2InstanceID {
			memberID = member.ID
		}
	}

	if memberID == "" {
		log.WithField("InstanceID", m.EC2InstanceID).Warn("received termination event for non-member")
		return true, nil
	}

	log.WithFields(log.Fields{
		"InstanceID": m.EC2InstanceID,
		"MemberID":   memberID}).Info("removing from cluster")
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("%s/v2/members/%s", etcdLocalURL, memberID), nil)
	_, err = client.Do(req)
	if err != nil {
		return false, err
	}

	return false, nil
}

func watchLifecycleEvents(s *ec2cluster.Cluster, localInstance *ec2.Instance) {
	etcdLocalURL = fmt.Sprintf("https://%s:2379", *localInstance.PrivateDnsName)
	for {
		err := s.WatchLifecycleEvents(handleLifecycleEvent)

		// The lifecycle hook might not exist yet if we're being created
		// by cloudformation.
		if err == ec2cluster.ErrLifecycleHookNotFound {
			log.Printf("WARNING: %s", err)
			time.Sleep(10 * time.Second)
			continue
		}
		if err != nil {
			log.Fatalf("ERROR: WatchLifecycleEvents: %s", err)
		}
		panic("not reached")
	}
}
