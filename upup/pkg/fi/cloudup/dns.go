/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloudup

import (
	"fmt"
	"github.com/golang/glog"
	api "k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/featureflag"
	"k8s.io/kops/pkg/model"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kubernetes/federation/pkg/dnsprovider"
	"k8s.io/kubernetes/federation/pkg/dnsprovider/rrstype"
	"net"
	"os"
	"strings"
)

const (
	// This IP is from TEST-NET-3
	// https://en.wikipedia.org/wiki/Reserved_IP_addresses
	PlaceholderIP  = "203.0.113.123"
	PlaceholderTTL = 10
)

func findZone(cluster *api.Cluster, cloud fi.Cloud) (dnsprovider.Zone, error) {
	dns, err := cloud.DNS()
	if err != nil {
		return nil, fmt.Errorf("error building DNS provider: %v", err)
	}

	zonesProvider, ok := dns.Zones()
	if !ok {
		return nil, fmt.Errorf("error getting DNS zones provider")
	}

	zones, err := zonesProvider.List()
	if err != nil {
		return nil, fmt.Errorf("error listing DNS zones: %v", err)
	}

	var matches []dnsprovider.Zone
	findName := strings.TrimSuffix(cluster.Spec.DNSZone, ".")
	for _, zone := range zones {
		id := zone.ID()
		name := strings.TrimSuffix(zone.Name(), ".")
		if id == cluster.Spec.DNSZone || name == findName {
			matches = append(matches, zone)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("cannot find DNS Zone %q.  Please pre-create the zone and set up NS records so that it resolves.", cluster.Spec.DNSZone)
	}

	if len(matches) > 1 {
		return nil, fmt.Errorf("found multiple DNS Zones matching %q", cluster.Spec.DNSZone)
	}

	zone := matches[0]
	return zone, nil
}

func validateDNS(cluster *api.Cluster, cloud fi.Cloud) error {
	kopsModelContext := &model.KopsModelContext{
		Cluster: cluster,
		// We are not initializing a lot of the fields here; revisit once UsePrivateDNS is "real"
	}

	if kopsModelContext.UsePrivateDNS() {
		glog.Infof("Private DNS: skipping DNS validation")
		return nil
	}

	zone, err := findZone(cluster, cloud)
	if err != nil {
		return err
	}
	dnsName := strings.TrimSuffix(zone.Name(), ".")

	glog.V(2).Infof("Doing DNS lookup to verify NS records for %q", dnsName)
	ns, err := net.LookupNS(dnsName)
	if err != nil {
		return fmt.Errorf("error doing DNS lookup for NS records for %q: %v", dnsName, err)
	}

	if len(ns) == 0 {
		if os.Getenv("DNS_IGNORE_NS_CHECK") == "" {
			return fmt.Errorf("NS records not found for %q - please make sure they are correctly configured", dnsName)
		} else {
			glog.Warningf("Ignoring failed NS record check because DNS_IGNORE_NS_CHECK is set")
		}
	} else {
		var hosts []string
		for _, n := range ns {
			hosts = append(hosts, n.Host)
		}
		glog.V(2).Infof("Found NS records for %q: %v", dnsName, hosts)
	}

	return nil
}

func precreateDNS(cluster *api.Cluster, cloud fi.Cloud) error {
	// TODO: Move to update
	if !featureflag.DNSPreCreate.Enabled() {
		glog.V(4).Infof("Skipping DNS record pre-creation because feature flag not enabled")
		return nil
	}

	// We precreate some DNS names (where they don't exist), with a dummy IP address
	// This avoids hitting negative TTL on DNS lookups, which tend to be very long
	// If we get the names wrong here, it doesn't really matter (extra DNS name, slower boot)

	dnsHostnames := buildPrecreateDNSHostnames(cluster)

	if len(dnsHostnames) == 0 {
		glog.Infof("No DNS records to pre-create")
		return nil
	}

	glog.Infof("Pre-creating DNS records")

	zone, err := findZone(cluster, cloud)
	if err != nil {
		return err
	}

	rrs, ok := zone.ResourceRecordSets()
	if !ok {
		return fmt.Errorf("error getting DNS resource records for %q", zone.Name())
	}

	records, err := rrs.List()
	if err != nil {
		return fmt.Errorf("error listing DNS resource records for %q: %v", zone.Name(), err)
	}

	recordsMap := make(map[string]dnsprovider.ResourceRecordSet)
	for _, record := range records {
		name := strings.TrimSuffix(record.Name(), ".")
		key := string(record.Type()) + "::" + name
		recordsMap[key] = record
	}

	changeset := rrs.StartChangeset()
	// TODO: Add ChangeSet.IsEmpty() method
	var created []string

	for _, dnsHostname := range dnsHostnames {
		dnsHostname = strings.TrimSuffix(dnsHostname, ".")
		dnsRecord := recordsMap["A::"+dnsHostname]
		found := false
		if dnsRecord != nil {
			rrdatas := dnsRecord.Rrdatas()
			if len(rrdatas) > 0 {
				glog.V(4).Infof("Found DNS record %s => %s; won't create", dnsHostname, rrdatas)
				found = true
			} else {
				// This is probably an alias target; leave it alone...
				glog.V(4).Infof("Found DNS record %s, but no records", dnsHostname)
				found = true
			}
		}

		if found {
			continue
		}

		glog.V(2).Infof("Pre-creating DNS record %s => %s", dnsHostname, PlaceholderIP)

		changeset.Add(rrs.New(dnsHostname, []string{PlaceholderIP}, PlaceholderTTL, rrstype.A))
		created = append(created, dnsHostname)
	}

	if len(created) != 0 {
		err := changeset.Apply()
		if err != nil {
			return fmt.Errorf("Error pre-creating DNS records: %v", err)
		}
		glog.V(2).Infof("Pre-created DNS names: %v", created)
	}

	return nil
}

// buildPrecreateDNSHostnames returns the hostnames we should precreate
func buildPrecreateDNSHostnames(cluster *api.Cluster) []string {
	dnsInternalSuffix := ".internal." + cluster.ObjectMeta.Name

	var dnsHostnames []string

	if cluster.Spec.MasterPublicName != "" {
		dnsHostnames = append(dnsHostnames, cluster.Spec.MasterPublicName)
	} else {
		glog.Warningf("cannot pre-create MasterPublicName - not set")
	}

	if cluster.Spec.MasterInternalName != "" {
		dnsHostnames = append(dnsHostnames, cluster.Spec.MasterInternalName)
	} else {
		glog.Warningf("cannot pre-create MasterInternalName - not set")
	}

	for _, etcdCluster := range cluster.Spec.EtcdClusters {
		etcClusterName := "etcd-" + etcdCluster.Name
		if etcdCluster.Name == "main" {
			// Special case
			etcClusterName = "etcd"
		}
		for _, etcdClusterMember := range etcdCluster.Members {
			name := etcClusterName + "-" + etcdClusterMember.Name + dnsInternalSuffix
			dnsHostnames = append(dnsHostnames, name)
		}
	}

	return dnsHostnames
}
