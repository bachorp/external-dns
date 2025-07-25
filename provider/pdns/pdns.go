/*
Copyright 2018 The Kubernetes Authors.

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

package pdns

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	pgo "github.com/ffledgling/pdns-go"
	log "github.com/sirupsen/logrus"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/pkg/tlsutils"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
)

type pdnsChangeType string

const (
	apiBase = "/api/v1"

	defaultTTL = 300

	// PdnsDelete and PdnsReplace are effectively an enum for "pgo.RrSet.changetype"
	// TODO: Can we somehow get this from the pgo swagger client library itself?

	// PdnsDelete : PowerDNS changetype used for deleting rrsets
	// ref: https://doc.powerdns.com/authoritative/http-api/zone.html#rrset (see "changetype")
	PdnsDelete pdnsChangeType = "DELETE"
	// PdnsReplace : PowerDNS changetype for creating, updating and patching rrsets
	PdnsReplace pdnsChangeType = "REPLACE"
	// Number of times to retry failed PDNS requests
	retryLimit = 3
	// time in milliseconds
	retryAfterTime = 250 * time.Millisecond
)

// PDNSConfig is comprised of the fields necessary to create a new PDNSProvider
type PDNSConfig struct {
	DomainFilter *endpoint.DomainFilter
	DryRun       bool
	Server       string
	ServerID     string
	APIKey       string
	TLSConfig    TLSConfig
}

// TLSConfig is comprised of the TLS-related fields necessary to create a new PDNSProvider
type TLSConfig struct {
	SkipTLSVerify         bool
	CAFilePath            string
	ClientCertFilePath    string
	ClientCertKeyFilePath string
}

func (tlsConfig *TLSConfig) setHTTPClient(pdnsClientConfig *pgo.Configuration) error {
	log.Debug("Configuring TLS for PDNS Provider.")
	tlsClientConfig, err := tlsutils.NewTLSConfig(
		tlsConfig.ClientCertFilePath,
		tlsConfig.ClientCertKeyFilePath,
		tlsConfig.CAFilePath,
		"",
		tlsConfig.SkipTLSVerify,
		tls.VersionTLS12,
	)
	if err != nil {
		return err
	}

	// Timeouts taken from net.http.DefaultTransport
	transporter := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       tlsClientConfig,
	}
	pdnsClientConfig.HTTPClient = &http.Client{
		Transport: transporter,
	}

	return nil
}

// Function for debug printing
func stringifyHTTPResponseBody(r *http.Response) string {
	if r == nil {
		return ""
	}

	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(r.Body)
	return buf.String()
}

// PDNSAPIProvider : Interface used and extended by the PDNSAPIClient struct as
// well as mock APIClients used in testing
type PDNSAPIProvider interface {
	ListZones() ([]pgo.Zone, *http.Response, error)
	PartitionZones(zones []pgo.Zone) ([]pgo.Zone, []pgo.Zone)
	ListZone(zoneID string) (pgo.Zone, *http.Response, error)
	PatchZone(zoneID string, zoneStruct pgo.Zone) (*http.Response, error)
}

// PDNSAPIClient : Struct that encapsulates all the PowerDNS specific implementation details
type PDNSAPIClient struct {
	dryRun       bool
	serverID     string
	authCtx      context.Context
	client       *pgo.APIClient
	domainFilter *endpoint.DomainFilter
}

// ListZones : Method returns all enabled zones from PowerDNS
// ref: https://doc.powerdns.com/authoritative/http-api/zone.html#get--servers-server_id-zones
func (c *PDNSAPIClient) ListZones() ([]pgo.Zone, *http.Response, error) {
	var zones []pgo.Zone
	var resp *http.Response
	var err error
	for i := 0; i < retryLimit; i++ {
		zones, resp, err = c.client.ZonesApi.ListZones(c.authCtx, c.serverID)
		if err != nil {
			log.Debugf("Unable to fetch zones %v", err)
			log.Debugf("Retrying ListZones() ... %d", i)
			time.Sleep(retryAfterTime * (1 << uint(i)))
			continue
		}
		return zones, resp, err
	}

	return zones, resp, provider.NewSoftErrorf("unable to list zones: %v", err)
}

// PartitionZones : Method returns a slice of zones that adhere to the domain filter and a slice of ones that does not adhere to the filter
func (c *PDNSAPIClient) PartitionZones(zones []pgo.Zone) ([]pgo.Zone, []pgo.Zone) {
	var filteredZones []pgo.Zone
	var residualZones []pgo.Zone

	if c.domainFilter.IsConfigured() {
		for _, zone := range zones {
			if c.domainFilter.Match(zone.Name) {
				filteredZones = append(filteredZones, zone)
			} else {
				residualZones = append(residualZones, zone)
			}
		}
	} else {
		filteredZones = zones
	}
	return filteredZones, residualZones
}

// ListZone : Method returns the details of a specific zone from PowerDNS
// ref: https://doc.powerdns.com/authoritative/http-api/zone.html#get--servers-server_id-zones-zone_id
func (c *PDNSAPIClient) ListZone(zoneID string) (pgo.Zone, *http.Response, error) {
	for i := 0; i < retryLimit; i++ {
		zone, resp, err := c.client.ZonesApi.ListZone(c.authCtx, c.serverID, zoneID)
		if err != nil {
			log.Debugf("Unable to fetch zone %v", err)
			log.Debugf("Retrying ListZone() ... %d", i)
			time.Sleep(retryAfterTime * (1 << uint(i)))
			continue
		}
		return zone, resp, err
	}

	return pgo.Zone{}, nil, provider.NewSoftErrorf("unable to list zone")
}

// PatchZone : Method used to update the contents of a particular zone from PowerDNS
// ref: https://doc.powerdns.com/authoritative/http-api/zone.html#patch--servers-server_id-zones-zone_id
func (c *PDNSAPIClient) PatchZone(zoneID string, zoneStruct pgo.Zone) (*http.Response, error) {
	var resp *http.Response
	var err error
	for i := 0; i < retryLimit; i++ {
		resp, err = c.client.ZonesApi.PatchZone(c.authCtx, c.serverID, zoneID, zoneStruct)
		if err != nil {
			log.Debugf("Unable to patch zone %v", err)
			log.Debugf("Retrying PatchZone() ... %d", i)
			time.Sleep(retryAfterTime * (1 << uint(i)))
			continue
		}
		return resp, err
	}

	return resp, provider.NewSoftErrorf("unable to patch zone: %v", err)
}

// PDNSProvider is an implementation of the Provider interface for PowerDNS
type PDNSProvider struct {
	provider.BaseProvider
	client PDNSAPIProvider
}

// NewPDNSProvider initializes a new PowerDNS based Provider.
func NewPDNSProvider(ctx context.Context, config PDNSConfig) (*PDNSProvider, error) {
	// Do some input validation

	if config.APIKey == "" {
		return nil, errors.New("missing API Key for PDNS. Specify using --pdns-api-key=")
	}

	// We do not support dry running, exit safely instead of surprising the user
	// TODO: Add Dry Run support
	if config.DryRun {
		return nil, errors.New("PDNS Provider does not currently support dry-run")
	}

	if config.Server == "localhost" {
		log.Warnf("PDNS Server is set to localhost, this may not be what you want. Specify using --pdns-server=")
	}

	pdnsClientConfig := pgo.NewConfiguration()
	pdnsClientConfig.BasePath = config.Server + apiBase
	if err := config.TLSConfig.setHTTPClient(pdnsClientConfig); err != nil {
		return nil, err
	}

	provider := &PDNSProvider{
		client: &PDNSAPIClient{
			dryRun:       config.DryRun,
			serverID:     config.ServerID,
			authCtx:      context.WithValue(ctx, pgo.ContextAPIKey, pgo.APIKey{Key: config.APIKey}),
			client:       pgo.NewAPIClient(pdnsClientConfig),
			domainFilter: config.DomainFilter,
		},
	}
	return provider, nil
}

func (p *PDNSProvider) convertRRSetToEndpoints(rr pgo.RrSet) ([]*endpoint.Endpoint, error) {
	endpoints := make([]*endpoint.Endpoint, 0)
	targets := make([]string, 0)
	rrType_ := rr.Type_

	for _, record := range rr.Records {
		// If a record is "Disabled", it's not supposed to be "visible"
		if !record.Disabled {
			targets = append(targets, record.Content)
		}
	}
	if rr.Type_ == "ALIAS" {
		rrType_ = "CNAME"
	}
	endpoints = append(endpoints, endpoint.NewEndpointWithTTL(rr.Name, rrType_, endpoint.TTL(rr.Ttl), targets...))
	return endpoints, nil
}

// ConvertEndpointsToZones marshals endpoints into pdns compatible Zone structs
func (p *PDNSProvider) ConvertEndpointsToZones(eps []*endpoint.Endpoint, changetype pdnsChangeType) ([]pgo.Zone, error) {
	var zoneList = make([]pgo.Zone, 0)
	endpoints := make([]*endpoint.Endpoint, len(eps))
	copy(endpoints, eps)

	// Sort the endpoints array so we have deterministic inserts
	sort.SliceStable(endpoints,
		func(i, j int) bool {
			// We only care about sorting endpoints with the same dnsname
			if endpoints[i].DNSName == endpoints[j].DNSName {
				return endpoints[i].RecordType < endpoints[j].RecordType
			}
			return endpoints[i].DNSName < endpoints[j].DNSName
		})

	zones, _, err := p.client.ListZones()
	if err != nil {
		return nil, err
	}
	filteredZones, residualZones := p.client.PartitionZones(zones)

	// Sort the zone by length of the name in descending order, we use this
	// property later to ensure we add a record to the longest matching zone

	sort.SliceStable(filteredZones, func(i, j int) bool { return len(filteredZones[i].Name) > len(filteredZones[j].Name) })

	// NOTE: Complexity of this loop is O(FilteredZones*Endpoints).
	// A possibly faster implementation would be a search of the reversed
	// DNSName in a trie of Zone names, which should be O(Endpoints), but at this point it's not
	// necessary.
	for _, zone := range filteredZones {
		zone.Rrsets = []pgo.RrSet{}
		for i := 0; i < len(endpoints); {
			ep := endpoints[i]
			dnsname := provider.EnsureTrailingDot(ep.DNSName)
			if dnsname == zone.Name || strings.HasSuffix(dnsname, "."+zone.Name) {
				// The assumption here is that there will only ever be one target
				// per (ep.DNSName, ep.RecordType) tuple, which holds true for
				// external-dns v5.0.0-alpha onwards
				records := []pgo.Record{}
				RecordType_ := ep.RecordType
				for _, t := range ep.Targets {
					if ep.RecordType == "CNAME" || ep.RecordType == "ALIAS" || ep.RecordType == "MX" || ep.RecordType == "SRV" {
						t = provider.EnsureTrailingDot(t)
					}
					records = append(records, pgo.Record{Content: t})
				}

				if dnsname == zone.Name && ep.RecordType == "CNAME" {
					log.Debugf("Converting APEX record %s from CNAME to ALIAS", dnsname)
					RecordType_ = "ALIAS"
				}

				rrset := pgo.RrSet{
					Name:       dnsname,
					Type_:      RecordType_,
					Records:    records,
					Changetype: string(changetype),
				}

				// DELETEs explicitly forbid a TTL, therefore only PATCHes need the TTL
				if changetype == PdnsReplace {
					if int64(ep.RecordTTL) > int64(math.MaxInt32) {
						return nil, provider.NewSoftError(fmt.Errorf("value of record TTL overflows, limited to int32"))
					}
					if ep.RecordTTL == 0 {
						// No TTL was specified for the record, we use the default
						rrset.Ttl = int32(defaultTTL)
					} else {
						rrset.Ttl = int32(ep.RecordTTL)
					}
				}

				zone.Rrsets = append(zone.Rrsets, rrset)

				// "pop" endpoint if it's matched
				endpoints = append(endpoints[0:i], endpoints[i+1:]...)
			} else {
				// If we didn't pop anything, we move to the next item in the list
				i++
			}
		}
		if len(zone.Rrsets) > 0 {
			zoneList = append(zoneList, zone)
		}
	}

	// residualZones is unsorted by name length like its counterpart
	// since we only care to remove endpoints that do not match domain filter
	for _, zone := range residualZones {
		for i := 0; i < len(endpoints); {
			ep := endpoints[i]
			dnsname := provider.EnsureTrailingDot(ep.DNSName)
			if dnsname == zone.Name || strings.HasSuffix(dnsname, "."+zone.Name) {
				// "pop" endpoint if it's matched to a residual zone... essentially a no-op
				log.Debugf("Ignoring Endpoint because it was matched to a zone that was not specified within Domain Filter(s): %s", dnsname)
				endpoints = append(endpoints[0:i], endpoints[i+1:]...)
			} else {
				i++
			}
		}
	}
	// If we still have some endpoints left, it means we couldn't find a matching zone (filtered or residual) for them
	// We warn instead of hard fail here because we don't want a misconfig to cause everything to go down
	if len(endpoints) > 0 {
		log.Warnf("No matching zones were found for the following endpoints: %+v", endpoints)
	}

	log.Debugf("Zone List generated from Endpoints: %+v", zoneList)

	return zoneList, nil
}

// mutateRecords takes a list of endpoints and creates, replaces or deletes them based on the changetype
func (p *PDNSProvider) mutateRecords(endpoints []*endpoint.Endpoint, changetype pdnsChangeType) error {
	zonelist, err := p.ConvertEndpointsToZones(endpoints, changetype)
	if err != nil {
		return err
	}
	for _, zone := range zonelist {
		jso, err := json.Marshal(zone)
		if err != nil {
			log.Errorf("JSON Marshal for zone struct failed!")
		} else {
			log.Debugf("Struct for PatchZone:\n%s", string(jso))
		}
		resp, err := p.client.PatchZone(zone.Id, zone)
		if err != nil {
			log.Debugf("PDNS API response: %s", stringifyHTTPResponseBody(resp))
			return err
		}
	}
	return nil
}

// Records returns all DNS records controlled by the configured PDNS server (for all zones)
func (p *PDNSProvider) Records(_ context.Context) ([]*endpoint.Endpoint, error) {
	zones, _, err := p.client.ListZones()
	if err != nil {
		return nil, err
	}
	filteredZones, _ := p.client.PartitionZones(zones)

	var endpoints []*endpoint.Endpoint

	for _, zone := range filteredZones {
		z, _, err := p.client.ListZone(zone.Id)
		if err != nil {
			return nil, provider.NewSoftErrorf("unable to fetch records: %v", err)
		}

		for _, rr := range z.Rrsets {
			e, err := p.convertRRSetToEndpoints(rr)
			if err != nil {
				return nil, err
			}
			endpoints = append(endpoints, e...)
		}
	}

	log.Debugf("Records fetched:\n%+v", endpoints)
	return endpoints, nil
}

// AdjustEndpoints performs checks on the provided endpoints and will skip any potentially failing changes.
func (p *PDNSProvider) AdjustEndpoints(endpoints []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) {
	var validEndpoints []*endpoint.Endpoint
	for i := 0; i < len(endpoints); i++ {
		if !endpoints[i].CheckEndpoint() {
			log.Warnf("Ignoring Endpoint because of invalid %v record formatting: {Target: '%v'}", endpoints[i].RecordType, endpoints[i].Targets)
			continue
		}
		validEndpoints = append(validEndpoints, endpoints[i])
	}
	return validEndpoints, nil
}

// ApplyChanges takes a list of changes (endpoints) and updates the PDNS server
// by sending the correct HTTP PATCH requests to a matching zone
func (p *PDNSProvider) ApplyChanges(ctx context.Context, changes *plan.Changes) error {
	startTime := time.Now()

	// Create
	for _, change := range changes.Create {
		log.Infof("CREATE: %+v", change)
	}
	// We only attempt to mutate records if there are any to mutate.  A
	// call to mutate records with an empty list of endpoints is still a
	// valid call and a no-op, but we might as well not make the call to
	// prevent unnecessary logging
	if len(changes.Create) > 0 {
		// "Replacing" non-existent records creates them
		err := p.mutateRecords(changes.Create, PdnsReplace)
		if err != nil {
			return err
		}
	}

	// Update
	for _, change := range changes.UpdateOld() {
		// Since PDNS "Patches", we don't need to specify the "old"
		// record. The Update New change type will automatically take
		// care of replacing the old RRSet with the new one We simply
		// leave this logging here for information
		log.Debugf("UPDATE-OLD (ignored): %+v", change)
	}

	updateNew := changes.UpdateNew()
	for _, change := range updateNew {
		log.Infof("UPDATE-NEW: %+v", change)
	}
	if len(updateNew) > 0 {
		err := p.mutateRecords(updateNew, PdnsReplace)
		if err != nil {
			return err
		}
	}

	// Delete
	for _, change := range changes.Delete {
		log.Infof("DELETE: %+v", change)
	}
	if len(changes.Delete) > 0 {
		err := p.mutateRecords(changes.Delete, PdnsDelete)
		if err != nil {
			return err
		}
	}
	log.Infof("Changes pushed out to PowerDNS in %s\n", time.Since(startTime))
	return nil
}
