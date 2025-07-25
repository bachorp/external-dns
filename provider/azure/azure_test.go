/*
Copyright 2017 The Kubernetes Authors.

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

package azure

import (
	"context"
	"testing"

	azcoreruntime "github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	dns "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/dns/armdns"
	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/internal/testutils"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
)

// mockZonesClient implements the methods of the Azure DNS Zones Client which are used in the Azure Provider
// and returns static results which are defined per test
type mockZonesClient struct {
	pagingHandler azcoreruntime.PagingHandler[dns.ZonesClientListByResourceGroupResponse]
}

func newMockZonesClient(zones []*dns.Zone) mockZonesClient {
	pagingHandler := azcoreruntime.PagingHandler[dns.ZonesClientListByResourceGroupResponse]{
		More: func(resp dns.ZonesClientListByResourceGroupResponse) bool {
			return false
		},
		Fetcher: func(context.Context, *dns.ZonesClientListByResourceGroupResponse) (dns.ZonesClientListByResourceGroupResponse, error) {
			return dns.ZonesClientListByResourceGroupResponse{
				ZoneListResult: dns.ZoneListResult{
					Value: zones,
				},
			}, nil
		},
	}
	return mockZonesClient{
		pagingHandler: pagingHandler,
	}
}

func (client *mockZonesClient) NewListByResourceGroupPager(resourceGroupName string, options *dns.ZonesClientListByResourceGroupOptions) *azcoreruntime.Pager[dns.ZonesClientListByResourceGroupResponse] {
	return azcoreruntime.NewPager(client.pagingHandler)
}

// mockZonesClient implements the methods of the Azure DNS RecordSet Client which are used in the Azure Provider
// and returns static results which are defined per test
type mockRecordSetsClient struct {
	pagingHandler    azcoreruntime.PagingHandler[dns.RecordSetsClientListAllByDNSZoneResponse]
	deletedEndpoints []*endpoint.Endpoint
	updatedEndpoints []*endpoint.Endpoint
}

func newMockRecordSetsClient(recordSets []*dns.RecordSet) mockRecordSetsClient {
	pagingHandler := azcoreruntime.PagingHandler[dns.RecordSetsClientListAllByDNSZoneResponse]{
		More: func(resp dns.RecordSetsClientListAllByDNSZoneResponse) bool {
			return false
		},
		Fetcher: func(context.Context, *dns.RecordSetsClientListAllByDNSZoneResponse) (dns.RecordSetsClientListAllByDNSZoneResponse, error) {
			return dns.RecordSetsClientListAllByDNSZoneResponse{
				RecordSetListResult: dns.RecordSetListResult{
					Value: recordSets,
				},
			}, nil
		},
	}
	return mockRecordSetsClient{
		pagingHandler: pagingHandler,
	}
}

func (client *mockRecordSetsClient) NewListAllByDNSZonePager(resourceGroupName string, zoneName string, options *dns.RecordSetsClientListAllByDNSZoneOptions) *azcoreruntime.Pager[dns.RecordSetsClientListAllByDNSZoneResponse] {
	return azcoreruntime.NewPager(client.pagingHandler)
}

func (client *mockRecordSetsClient) Delete(ctx context.Context, resourceGroupName string, zoneName string, relativeRecordSetName string, recordType dns.RecordType, options *dns.RecordSetsClientDeleteOptions) (dns.RecordSetsClientDeleteResponse, error) {
	client.deletedEndpoints = append(
		client.deletedEndpoints,
		endpoint.NewEndpoint(
			formatAzureDNSName(relativeRecordSetName, zoneName),
			string(recordType),
			"",
		),
	)
	return dns.RecordSetsClientDeleteResponse{}, nil
}

func (client *mockRecordSetsClient) CreateOrUpdate(ctx context.Context, resourceGroupName string, zoneName string, relativeRecordSetName string, recordType dns.RecordType, parameters dns.RecordSet, options *dns.RecordSetsClientCreateOrUpdateOptions) (dns.RecordSetsClientCreateOrUpdateResponse, error) {
	var ttl endpoint.TTL
	if parameters.Properties.TTL != nil {
		ttl = endpoint.TTL(*parameters.Properties.TTL)
	}
	client.updatedEndpoints = append(
		client.updatedEndpoints,
		endpoint.NewEndpointWithTTL(
			formatAzureDNSName(relativeRecordSetName, zoneName),
			string(recordType),
			ttl,
			extractAzureTargets(&parameters)...,
		),
	)
	return dns.RecordSetsClientCreateOrUpdateResponse{}, nil
}

func createMockZone(zone string, id string) *dns.Zone {
	return &dns.Zone{
		ID:   to.Ptr(id),
		Name: to.Ptr(zone),
	}
}

func aRecordSetPropertiesGetter(values []string, ttl int64) *dns.RecordSetProperties {
	aRecords := make([]*dns.ARecord, len(values))
	for i, value := range values {
		aRecords[i] = &dns.ARecord{
			IPv4Address: to.Ptr(value),
		}
	}
	return &dns.RecordSetProperties{
		TTL:      to.Ptr(ttl),
		ARecords: aRecords,
	}
}

func aaaaRecordSetPropertiesGetter(values []string, ttl int64) *dns.RecordSetProperties {
	aaaaRecords := make([]*dns.AaaaRecord, len(values))
	for i, value := range values {
		aaaaRecords[i] = &dns.AaaaRecord{
			IPv6Address: to.Ptr(value),
		}
	}
	return &dns.RecordSetProperties{
		TTL:         to.Ptr(ttl),
		AaaaRecords: aaaaRecords,
	}
}

func cNameRecordSetPropertiesGetter(values []string, ttl int64) *dns.RecordSetProperties {
	return &dns.RecordSetProperties{
		TTL: to.Ptr(ttl),
		CnameRecord: &dns.CnameRecord{
			Cname: to.Ptr(values[0]),
		},
	}
}

func mxRecordSetPropertiesGetter(values []string, ttl int64) *dns.RecordSetProperties {
	mxRecords := make([]*dns.MxRecord, len(values))
	for i, target := range values {
		mxRecord, _ := parseMxTarget[dns.MxRecord](target)
		mxRecords[i] = &mxRecord
	}
	return &dns.RecordSetProperties{
		TTL:       to.Ptr(ttl),
		MxRecords: mxRecords,
	}
}

func nsRecordSetPropertiesGetter(values []string, ttl int64) *dns.RecordSetProperties {
	nsRecords := make([]*dns.NsRecord, len(values))
	for i, value := range values {
		nsRecords[i] = &dns.NsRecord{
			Nsdname: to.Ptr(value),
		}
	}
	return &dns.RecordSetProperties{
		TTL:       to.Ptr(ttl),
		NsRecords: nsRecords,
	}
}

func txtRecordSetPropertiesGetter(values []string, ttl int64) *dns.RecordSetProperties {
	return &dns.RecordSetProperties{
		TTL: to.Ptr(ttl),
		TxtRecords: []*dns.TxtRecord{
			{
				Value: []*string{to.Ptr(values[0])},
			},
		},
	}
}

func othersRecordSetPropertiesGetter(values []string, ttl int64) *dns.RecordSetProperties {
	return &dns.RecordSetProperties{
		TTL: to.Ptr(ttl),
	}
}

func createMockRecordSet(name, recordType string, values ...string) *dns.RecordSet {
	return createMockRecordSetMultiWithTTL(name, recordType, 0, values...)
}

func createMockRecordSetWithTTL(name, recordType, value string, ttl int64) *dns.RecordSet {
	return createMockRecordSetMultiWithTTL(name, recordType, ttl, value)
}

func createMockRecordSetMultiWithTTL(name, recordType string, ttl int64, values ...string) *dns.RecordSet {
	var getterFunc func(values []string, ttl int64) *dns.RecordSetProperties

	switch recordType {
	case endpoint.RecordTypeA:
		getterFunc = aRecordSetPropertiesGetter
	case endpoint.RecordTypeAAAA:
		getterFunc = aaaaRecordSetPropertiesGetter
	case endpoint.RecordTypeCNAME:
		getterFunc = cNameRecordSetPropertiesGetter
	case endpoint.RecordTypeMX:
		getterFunc = mxRecordSetPropertiesGetter
	case endpoint.RecordTypeNS:
		getterFunc = nsRecordSetPropertiesGetter
	case endpoint.RecordTypeTXT:
		getterFunc = txtRecordSetPropertiesGetter
	default:
		getterFunc = othersRecordSetPropertiesGetter
	}
	return &dns.RecordSet{
		Name:       to.Ptr(name),
		Type:       to.Ptr("Microsoft.Network/dnszones/" + recordType),
		Properties: getterFunc(values, ttl),
	}
}

// newMockedAzureProvider creates an AzureProvider comprising the mocked clients for zones and recordsets
func newMockedAzureProvider(domainFilter *endpoint.DomainFilter, zoneNameFilter *endpoint.DomainFilter, zoneIDFilter provider.ZoneIDFilter, dryRun bool, resourceGroup string, userAssignedIdentityClientID string, activeDirectoryAuthorityHost string, zones []*dns.Zone, recordSets []*dns.RecordSet, maxRetriesCount int) (*AzureProvider, error) {
	zonesClient := newMockZonesClient(zones)
	recordSetsClient := newMockRecordSetsClient(recordSets)
	return newAzureProvider(domainFilter, zoneNameFilter, zoneIDFilter, dryRun, resourceGroup, userAssignedIdentityClientID, activeDirectoryAuthorityHost, &zonesClient, &recordSetsClient, maxRetriesCount), nil
}

func newAzureProvider(domainFilter *endpoint.DomainFilter, zoneNameFilter *endpoint.DomainFilter, zoneIDFilter provider.ZoneIDFilter, dryRun bool, resourceGroup string, userAssignedIdentityClientID string, activeDirectoryAuthorityHost string, zonesClient ZonesClient, recordsClient RecordSetsClient, maxRetriesCount int) *AzureProvider {
	return &AzureProvider{
		domainFilter:                 domainFilter,
		zoneNameFilter:               zoneNameFilter,
		zoneIDFilter:                 zoneIDFilter,
		dryRun:                       dryRun,
		resourceGroup:                resourceGroup,
		userAssignedIdentityClientID: userAssignedIdentityClientID,
		activeDirectoryAuthorityHost: activeDirectoryAuthorityHost,
		zonesClient:                  zonesClient,
		zonesCache:                   &zonesCache[dns.Zone]{duration: 0},
		recordSetsClient:             recordsClient,
		maxRetriesCount:              maxRetriesCount,
	}
}

func validateAzureEndpoints(t *testing.T, endpoints []*endpoint.Endpoint, expected []*endpoint.Endpoint) {
	assert.True(t, testutils.SameEndpoints(endpoints, expected), "actual and expected endpoints don't match. %s:%s", endpoints, expected)
}

func TestAzureRecord(t *testing.T) {
	provider, err := newMockedAzureProvider(endpoint.NewDomainFilter([]string{"example.com"}), endpoint.NewDomainFilter([]string{}), provider.NewZoneIDFilter([]string{""}), true, "k8s", "", "",
		[]*dns.Zone{
			createMockZone("example.com", "/dnszones/example.com"),
		},
		[]*dns.RecordSet{
			createMockRecordSet("@", endpoint.RecordTypeNS, "ns1-03.azure-dns.com."),
			createMockRecordSet("@", "SOA", "Email: azuredns-hostmaster.microsoft.com"),
			createMockRecordSet("@", endpoint.RecordTypeA, "123.123.123.122"),
			createMockRecordSet("@", endpoint.RecordTypeAAAA, "2001::123:123:123:122"),
			createMockRecordSet("cloud", endpoint.RecordTypeNS, "ns1.example.com."),
			createMockRecordSet("@", endpoint.RecordTypeTXT, "heritage=external-dns,external-dns/owner=default"),
			createMockRecordSetWithTTL("nginx", endpoint.RecordTypeA, "123.123.123.123", 3600),
			createMockRecordSetWithTTL("nginx", endpoint.RecordTypeAAAA, "2001::123:123:123:123", 3600),
			createMockRecordSetWithTTL("cloud-ttl", endpoint.RecordTypeNS, "ns1-ttl.example.com.", 10),
			createMockRecordSetWithTTL("nginx", endpoint.RecordTypeTXT, "heritage=external-dns,external-dns/owner=default", recordTTL),
			createMockRecordSetWithTTL("hack", endpoint.RecordTypeCNAME, "hack.azurewebsites.net", 10),
			createMockRecordSetMultiWithTTL("mail", endpoint.RecordTypeMX, 4000, "10 example.com"),
		}, 3)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	actual, err := provider.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expected := []*endpoint.Endpoint{
		endpoint.NewEndpoint("example.com", endpoint.RecordTypeNS, "ns1-03.azure-dns.com."),
		endpoint.NewEndpoint("example.com", endpoint.RecordTypeA, "123.123.123.122"),
		endpoint.NewEndpoint("example.com", endpoint.RecordTypeAAAA, "2001::123:123:123:122"),
		endpoint.NewEndpoint("cloud.example.com", endpoint.RecordTypeNS, "ns1.example.com."),
		endpoint.NewEndpoint("example.com", endpoint.RecordTypeTXT, "heritage=external-dns,external-dns/owner=default"),
		endpoint.NewEndpointWithTTL("nginx.example.com", endpoint.RecordTypeA, 3600, "123.123.123.123"),
		endpoint.NewEndpointWithTTL("nginx.example.com", endpoint.RecordTypeAAAA, 3600, "2001::123:123:123:123"),
		endpoint.NewEndpointWithTTL("cloud-ttl.example.com", endpoint.RecordTypeNS, 10, "ns1-ttl.example.com."),
		endpoint.NewEndpointWithTTL("nginx.example.com", endpoint.RecordTypeTXT, recordTTL, "heritage=external-dns,external-dns/owner=default"),
		endpoint.NewEndpointWithTTL("hack.example.com", endpoint.RecordTypeCNAME, 10, "hack.azurewebsites.net"),
		endpoint.NewEndpointWithTTL("mail.example.com", endpoint.RecordTypeMX, 4000, "10 example.com"),
	}

	validateAzureEndpoints(t, actual, expected)
}

func TestAzureMultiRecord(t *testing.T) {
	provider, err := newMockedAzureProvider(endpoint.NewDomainFilter([]string{"example.com"}), endpoint.NewDomainFilter([]string{}), provider.NewZoneIDFilter([]string{""}), true, "k8s", "", "",
		[]*dns.Zone{
			createMockZone("example.com", "/dnszones/example.com"),
		},
		[]*dns.RecordSet{
			createMockRecordSet("@", endpoint.RecordTypeNS, "ns1-03.azure-dns.com."),
			createMockRecordSet("@", "SOA", "Email: azuredns-hostmaster.microsoft.com"),
			createMockRecordSet("@", endpoint.RecordTypeA, "123.123.123.122", "234.234.234.233"),
			createMockRecordSet("@", endpoint.RecordTypeAAAA, "2001::123:123:123:122", "2001::234:234:234:233"),
			createMockRecordSet("cloud", endpoint.RecordTypeNS, "ns1.example.com.", "ns2.example.com."),
			createMockRecordSet("@", endpoint.RecordTypeTXT, "heritage=external-dns,external-dns/owner=default"),
			createMockRecordSetMultiWithTTL("nginx", endpoint.RecordTypeA, 3600, "123.123.123.123", "234.234.234.234"),
			createMockRecordSetMultiWithTTL("nginx", endpoint.RecordTypeAAAA, 3600, "2001::123:123:123:123", "2001::234:234:234:234"),
			createMockRecordSetMultiWithTTL("cloud-ttl", endpoint.RecordTypeNS, 10, "ns1-ttl.example.com.", "ns2-ttl.example.com."),
			createMockRecordSetWithTTL("nginx", endpoint.RecordTypeTXT, "heritage=external-dns,external-dns/owner=default", recordTTL),
			createMockRecordSetWithTTL("hack", endpoint.RecordTypeCNAME, "hack.azurewebsites.net", 10),
			createMockRecordSetMultiWithTTL("mail", endpoint.RecordTypeMX, 4000, "10 example.com", "20 backup.example.com"),
		}, 3)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	actual, err := provider.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expected := []*endpoint.Endpoint{
		endpoint.NewEndpoint("example.com", endpoint.RecordTypeNS, "ns1-03.azure-dns.com."),
		endpoint.NewEndpoint("example.com", endpoint.RecordTypeA, "123.123.123.122", "234.234.234.233"),
		endpoint.NewEndpoint("example.com", endpoint.RecordTypeAAAA, "2001::123:123:123:122", "2001::234:234:234:233"),
		endpoint.NewEndpoint("cloud.example.com", endpoint.RecordTypeNS, "ns1.example.com.", "ns2.example.com."),
		endpoint.NewEndpoint("example.com", endpoint.RecordTypeTXT, "heritage=external-dns,external-dns/owner=default"),
		endpoint.NewEndpointWithTTL("nginx.example.com", endpoint.RecordTypeA, 3600, "123.123.123.123", "234.234.234.234"),
		endpoint.NewEndpointWithTTL("nginx.example.com", endpoint.RecordTypeAAAA, 3600, "2001::123:123:123:123", "2001::234:234:234:234"),
		endpoint.NewEndpointWithTTL("cloud-ttl.example.com", endpoint.RecordTypeNS, 10, "ns1-ttl.example.com.", "ns2-ttl.example.com."),
		endpoint.NewEndpointWithTTL("nginx.example.com", endpoint.RecordTypeTXT, recordTTL, "heritage=external-dns,external-dns/owner=default"),
		endpoint.NewEndpointWithTTL("hack.example.com", endpoint.RecordTypeCNAME, 10, "hack.azurewebsites.net"),
		endpoint.NewEndpointWithTTL("mail.example.com", endpoint.RecordTypeMX, 4000, "10 example.com", "20 backup.example.com"),
	}

	validateAzureEndpoints(t, actual, expected)
}

func TestAzureApplyChanges(t *testing.T) {
	recordsClient := mockRecordSetsClient{}

	testAzureApplyChangesInternal(t, false, &recordsClient)

	validateAzureEndpoints(t, recordsClient.deletedEndpoints, []*endpoint.Endpoint{
		endpoint.NewEndpoint("deleted.example.com", endpoint.RecordTypeA, ""),
		endpoint.NewEndpoint("deletedaaaa.example.com", endpoint.RecordTypeAAAA, ""),
		endpoint.NewEndpoint("deletedcname.example.com", endpoint.RecordTypeCNAME, ""),
		endpoint.NewEndpoint("deletedns.example.com", endpoint.RecordTypeNS, ""),
	})

	validateAzureEndpoints(t, recordsClient.updatedEndpoints, []*endpoint.Endpoint{
		endpoint.NewEndpointWithTTL("example.com", endpoint.RecordTypeA, endpoint.TTL(recordTTL), "1.2.3.4"),
		endpoint.NewEndpointWithTTL("example.com", endpoint.RecordTypeAAAA, endpoint.TTL(recordTTL), "2001::1:2:3:4"),
		endpoint.NewEndpointWithTTL("example.com", endpoint.RecordTypeTXT, endpoint.TTL(recordTTL), "tag"),
		endpoint.NewEndpointWithTTL("foo.example.com", endpoint.RecordTypeA, endpoint.TTL(recordTTL), "1.2.3.4", "1.2.3.5"),
		endpoint.NewEndpointWithTTL("foo.example.com", endpoint.RecordTypeAAAA, endpoint.TTL(recordTTL), "2001::1:2:3:4", "2001::1:2:3:5"),
		endpoint.NewEndpointWithTTL("cloud.example.com", endpoint.RecordTypeNS, endpoint.TTL(recordTTL), "ns1.example.com."),
		endpoint.NewEndpointWithTTL("foo.example.com", endpoint.RecordTypeTXT, endpoint.TTL(recordTTL), "tag"),
		endpoint.NewEndpointWithTTL("bar.example.com", endpoint.RecordTypeCNAME, endpoint.TTL(recordTTL), "other.com"),
		endpoint.NewEndpointWithTTL("bar.example.com", endpoint.RecordTypeTXT, endpoint.TTL(recordTTL), "tag"),
		endpoint.NewEndpointWithTTL("other.com", endpoint.RecordTypeA, endpoint.TTL(recordTTL), "5.6.7.8"),
		endpoint.NewEndpointWithTTL("other.com", endpoint.RecordTypeAAAA, endpoint.TTL(recordTTL), "2001::5:6:7:8"),
		endpoint.NewEndpointWithTTL("cloud.other.com", endpoint.RecordTypeNS, endpoint.TTL(recordTTL), "ns2.other.com."),
		endpoint.NewEndpointWithTTL("other.com", endpoint.RecordTypeTXT, endpoint.TTL(recordTTL), "tag"),
		endpoint.NewEndpointWithTTL("new.example.com", endpoint.RecordTypeA, 3600, "111.222.111.222"),
		endpoint.NewEndpointWithTTL("new.example.com", endpoint.RecordTypeAAAA, 3600, "2001::111:222:111:222"),
		endpoint.NewEndpointWithTTL("newcname.example.com", endpoint.RecordTypeCNAME, 10, "other.com"),
		endpoint.NewEndpointWithTTL("newns.example.com", endpoint.RecordTypeNS, 10, "ns1.example.com."),
		endpoint.NewEndpointWithTTL("newmail.example.com", endpoint.RecordTypeMX, 7200, "40 bar.other.com"),
		endpoint.NewEndpointWithTTL("mail.example.com", endpoint.RecordTypeMX, endpoint.TTL(recordTTL), "10 other.com"),
		endpoint.NewEndpointWithTTL("mail.example.com", endpoint.RecordTypeTXT, endpoint.TTL(recordTTL), "tag"),
	})
}

func TestAzureApplyChangesDryRun(t *testing.T) {
	recordsClient := mockRecordSetsClient{}

	testAzureApplyChangesInternal(t, true, &recordsClient)

	validateAzureEndpoints(t, recordsClient.deletedEndpoints, []*endpoint.Endpoint{})

	validateAzureEndpoints(t, recordsClient.updatedEndpoints, []*endpoint.Endpoint{})
}

func testAzureApplyChangesInternal(t *testing.T, dryRun bool, client RecordSetsClient) {
	zones := []*dns.Zone{
		createMockZone("example.com", "/dnszones/example.com"),
		createMockZone("other.com", "/dnszones/other.com"),
	}
	zonesClient := newMockZonesClient(zones)

	provider := newAzureProvider(
		endpoint.NewDomainFilter([]string{""}),
		endpoint.NewDomainFilter([]string{""}),
		provider.NewZoneIDFilter([]string{""}),
		dryRun,
		"group",
		"",
		"",
		&zonesClient,
		client,
		3,
	)

	createRecords := []*endpoint.Endpoint{
		endpoint.NewEndpoint("example.com", endpoint.RecordTypeA, "1.2.3.4"),
		endpoint.NewEndpoint("example.com", endpoint.RecordTypeAAAA, "2001::1:2:3:4"),
		endpoint.NewEndpoint("example.com", endpoint.RecordTypeTXT, "tag"),
		endpoint.NewEndpoint("foo.example.com", endpoint.RecordTypeA, "1.2.3.5", "1.2.3.4"),
		endpoint.NewEndpoint("foo.example.com", endpoint.RecordTypeAAAA, "2001::1:2:3:5", "2001::1:2:3:4"),
		endpoint.NewEndpoint("cloud.example.com", endpoint.RecordTypeNS, "ns1.example.com."),
		endpoint.NewEndpoint("foo.example.com", endpoint.RecordTypeTXT, "tag"),
		endpoint.NewEndpoint("bar.example.com", endpoint.RecordTypeCNAME, "other.com"),
		endpoint.NewEndpoint("bar.example.com", endpoint.RecordTypeTXT, "tag"),
		endpoint.NewEndpoint("other.com", endpoint.RecordTypeA, "5.6.7.8"),
		endpoint.NewEndpoint("other.com", endpoint.RecordTypeAAAA, "2001::5:6:7:8"),
		endpoint.NewEndpoint("other.com", endpoint.RecordTypeTXT, "tag"),
		endpoint.NewEndpoint("cloud.other.com", endpoint.RecordTypeNS, "ns2.other.com."),
		endpoint.NewEndpoint("nope.com", endpoint.RecordTypeA, "4.4.4.4"),
		endpoint.NewEndpoint("nope.com", endpoint.RecordTypeAAAA, "2001::4:4:4:4"),
		endpoint.NewEndpoint("cloud.nope.com", endpoint.RecordTypeNS, "ns1.nope.com."),
		endpoint.NewEndpoint("nope.com", endpoint.RecordTypeTXT, "tag"),
		endpoint.NewEndpoint("mail.example.com", endpoint.RecordTypeMX, "10 other.com"),
		endpoint.NewEndpoint("mail.example.com", endpoint.RecordTypeTXT, "tag"),
	}

	currentRecords := []*endpoint.Endpoint{
		endpoint.NewEndpoint("old.example.com", endpoint.RecordTypeA, "121.212.121.212"),
		endpoint.NewEndpoint("old.example.com", endpoint.RecordTypeA, "2001::111:222:111:222"),
		endpoint.NewEndpoint("oldcname.example.com", endpoint.RecordTypeCNAME, "other.com"),
		endpoint.NewEndpoint("oldns.example.com", endpoint.RecordTypeNS, "ns1.example.com."),
		endpoint.NewEndpoint("old.nope.com", endpoint.RecordTypeA, "121.212.121.212"),
		endpoint.NewEndpoint("old.nope.com", endpoint.RecordTypeA, "121.212.121.212"),
		endpoint.NewEndpoint("oldns.nope.com", endpoint.RecordTypeNS, "ns1.example.com"),
		endpoint.NewEndpoint("oldmail.example.com", endpoint.RecordTypeMX, "20 foo.other.com"),
	}
	updatedRecords := []*endpoint.Endpoint{
		endpoint.NewEndpointWithTTL("new.example.com", endpoint.RecordTypeA, 3600, "111.222.111.222"),
		endpoint.NewEndpointWithTTL("new.example.com", endpoint.RecordTypeAAAA, 3600, "2001::111:222:111:222"),
		endpoint.NewEndpointWithTTL("newcname.example.com", endpoint.RecordTypeCNAME, 10, "other.com"),
		endpoint.NewEndpointWithTTL("newns.example.com", endpoint.RecordTypeNS, 10, "ns1.example.com."),
		endpoint.NewEndpoint("new.nope.com", endpoint.RecordTypeA, "222.111.222.111"),
		endpoint.NewEndpoint("new.nope.com", endpoint.RecordTypeAAAA, "2001::222:111:222:111"),
		endpoint.NewEndpoint("newns.nope.com", endpoint.RecordTypeNS, "ns1.example.com"),
		endpoint.NewEndpointWithTTL("newmail.example.com", endpoint.RecordTypeMX, 7200, "40 bar.other.com"),
	}

	deleteRecords := []*endpoint.Endpoint{
		endpoint.NewEndpoint("deleted.example.com", endpoint.RecordTypeA, "111.222.111.222"),
		endpoint.NewEndpoint("deletedaaaa.example.com", endpoint.RecordTypeAAAA, "2001::111:222:111:222"),
		endpoint.NewEndpoint("deletedcname.example.com", endpoint.RecordTypeCNAME, "other.com"),
		endpoint.NewEndpoint("deletedns.example.com", endpoint.RecordTypeNS, "ns1.example.com."),
		endpoint.NewEndpoint("deleted.nope.com", endpoint.RecordTypeA, "222.111.222.111"),
		endpoint.NewEndpoint("deleted.nope.com", endpoint.RecordTypeAAAA, "2001::222:111:222:111"),
		endpoint.NewEndpoint("deletedns.nope.com", endpoint.RecordTypeNS, "ns1.example.com."),
	}

	update, err := plan.MkUpdates(currentRecords, updatedRecords)
	assert.NoError(t, err)
	changes := &plan.Changes{
		Create: createRecords,
		Update: update,
		Delete: deleteRecords,
	}

	if err := provider.ApplyChanges(context.Background(), changes); err != nil {
		t.Fatal(err)
	}
}

func TestAzureNameFilter(t *testing.T) {
	provider, err := newMockedAzureProvider(endpoint.NewDomainFilter([]string{"nginx.example.com"}), endpoint.NewDomainFilter([]string{"example.com"}), provider.NewZoneIDFilter([]string{""}), true, "k8s", "", "",
		[]*dns.Zone{
			createMockZone("example.com", "/dnszones/example.com"),
		},

		[]*dns.RecordSet{
			createMockRecordSet("@", "NS", "ns1-03.azure-dns.com."),
			createMockRecordSet("@", "SOA", "Email: azuredns-hostmaster.microsoft.com"),
			createMockRecordSet("@", endpoint.RecordTypeA, "123.123.123.122"),
			createMockRecordSet("@", endpoint.RecordTypeTXT, "heritage=external-dns,external-dns/owner=default"),
			createMockRecordSetWithTTL("test.nginx", endpoint.RecordTypeA, "123.123.123.123", 3600),
			createMockRecordSetWithTTL("nginx", endpoint.RecordTypeA, "123.123.123.123", 3600),
			createMockRecordSetWithTTL("nginx", endpoint.RecordTypeNS, "ns1.example.com.", 3600),
			createMockRecordSetWithTTL("nginx", endpoint.RecordTypeTXT, "heritage=external-dns,external-dns/owner=default", recordTTL),
			createMockRecordSetWithTTL("mail.nginx", endpoint.RecordTypeMX, "20 example.com", recordTTL),
			createMockRecordSetWithTTL("hack", endpoint.RecordTypeCNAME, "hack.azurewebsites.net", 10),
			createMockRecordSetWithTTL("hack", endpoint.RecordTypeNS, "ns1.example.com.", 3600),
		}, 3)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	actual, err := provider.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expected := []*endpoint.Endpoint{
		endpoint.NewEndpointWithTTL("test.nginx.example.com", endpoint.RecordTypeA, 3600, "123.123.123.123"),
		endpoint.NewEndpointWithTTL("nginx.example.com", endpoint.RecordTypeA, 3600, "123.123.123.123"),
		endpoint.NewEndpointWithTTL("nginx.example.com", endpoint.RecordTypeNS, 3600, "ns1.example.com."),
		endpoint.NewEndpointWithTTL("nginx.example.com", endpoint.RecordTypeTXT, recordTTL, "heritage=external-dns,external-dns/owner=default"),
		endpoint.NewEndpointWithTTL("mail.nginx.example.com", endpoint.RecordTypeMX, recordTTL, "20 example.com"),
	}

	validateAzureEndpoints(t, actual, expected)
}

func TestAzureApplyChangesZoneName(t *testing.T) {
	recordsClient := mockRecordSetsClient{}

	testAzureApplyChangesInternalZoneName(t, false, &recordsClient)

	validateAzureEndpoints(t, recordsClient.deletedEndpoints, []*endpoint.Endpoint{
		endpoint.NewEndpoint("deleted.foo.example.com", endpoint.RecordTypeA, ""),
		endpoint.NewEndpoint("deletedaaaa.foo.example.com", endpoint.RecordTypeAAAA, ""),
		endpoint.NewEndpoint("deletedcname.foo.example.com", endpoint.RecordTypeCNAME, ""),
		endpoint.NewEndpoint("deletedns.foo.example.com", endpoint.RecordTypeNS, ""),
	})

	validateAzureEndpoints(t, recordsClient.updatedEndpoints, []*endpoint.Endpoint{
		endpoint.NewEndpointWithTTL("foo.example.com", endpoint.RecordTypeA, endpoint.TTL(recordTTL), "1.2.3.4", "1.2.3.5"),
		endpoint.NewEndpointWithTTL("foo.example.com", endpoint.RecordTypeAAAA, endpoint.TTL(recordTTL), "2001::1:2:3:4", "2001::1:2:3:5"),
		endpoint.NewEndpointWithTTL("foo.example.com", endpoint.RecordTypeNS, endpoint.TTL(recordTTL), "ns1.example.com."),
		endpoint.NewEndpointWithTTL("foo.example.com", endpoint.RecordTypeTXT, endpoint.TTL(recordTTL), "tag"),
		endpoint.NewEndpointWithTTL("new.foo.example.com", endpoint.RecordTypeA, 3600, "111.222.111.222"),
		endpoint.NewEndpointWithTTL("new.foo.example.com", endpoint.RecordTypeAAAA, 3600, "2001::111:222:111:222"),
		endpoint.NewEndpointWithTTL("newns.foo.example.com", endpoint.RecordTypeNS, 10, "ns1.foo.example.com."),
		endpoint.NewEndpointWithTTL("newcname.foo.example.com", endpoint.RecordTypeCNAME, 10, "other.com"),
	})
}

func testAzureApplyChangesInternalZoneName(t *testing.T, dryRun bool, client RecordSetsClient) {
	zonesClient := newMockZonesClient([]*dns.Zone{createMockZone("example.com", "/dnszones/example.com")})

	provider := newAzureProvider(
		endpoint.NewDomainFilter([]string{"foo.example.com"}),
		endpoint.NewDomainFilter([]string{"example.com"}),
		provider.NewZoneIDFilter([]string{""}),
		dryRun,
		"group",
		"",
		"",
		&zonesClient,
		client,
		3,
	)

	createRecords := []*endpoint.Endpoint{
		endpoint.NewEndpoint("example.com", endpoint.RecordTypeA, "1.2.3.4"),
		endpoint.NewEndpoint("example.com", endpoint.RecordTypeAAAA, "2001::1:2:3:4"),
		endpoint.NewEndpoint("example.com", endpoint.RecordTypeTXT, "tag"),
		endpoint.NewEndpoint("foo.example.com", endpoint.RecordTypeA, "1.2.3.5", "1.2.3.4"),
		endpoint.NewEndpoint("foo.example.com", endpoint.RecordTypeAAAA, "2001::1:2:3:5", "2001::1:2:3:4"),
		endpoint.NewEndpoint("foo.example.com", endpoint.RecordTypeNS, "ns1.example.com."),
		endpoint.NewEndpoint("foo.example.com", endpoint.RecordTypeTXT, "tag"),
		endpoint.NewEndpoint("bar.example.com", endpoint.RecordTypeCNAME, "other.com"),
		endpoint.NewEndpoint("barns.example.com", endpoint.RecordTypeNS, "ns1.example.com."),
		endpoint.NewEndpoint("bar.example.com", endpoint.RecordTypeTXT, "tag"),
		endpoint.NewEndpoint("other.com", endpoint.RecordTypeA, "5.6.7.8"),
		endpoint.NewEndpoint("foons.other.com", endpoint.RecordTypeNS, "ns1.other.com"),
		endpoint.NewEndpoint("other.com", endpoint.RecordTypeTXT, "tag"),
		endpoint.NewEndpoint("nope.com", endpoint.RecordTypeA, "4.4.4.4"),
		endpoint.NewEndpoint("nope.com", endpoint.RecordTypeTXT, "tag"),
	}

	currentRecords := []*endpoint.Endpoint{
		endpoint.NewEndpoint("old.foo.example.com", endpoint.RecordTypeA, "121.212.121.212"),
		endpoint.NewEndpoint("old.foo.example.com", endpoint.RecordTypeAAAA, "2001::111:222:111:222"),
		endpoint.NewEndpoint("oldcname.foo.example.com", endpoint.RecordTypeCNAME, "other.com"),
		endpoint.NewEndpoint("oldns.foo.example.com", endpoint.RecordTypeNS, "ns1.foo.example.com."),
		endpoint.NewEndpoint("old.nope.example.com", endpoint.RecordTypeA, "121.212.121.212"),
		endpoint.NewEndpoint("old.nope.example.com", endpoint.RecordTypeAAAA, "2001::222:111:222:111"),
		endpoint.NewEndpoint("oldns.nope.example.com", endpoint.RecordTypeNS, "ns1.nope.example.com."),
	}
	updatedRecords := []*endpoint.Endpoint{
		endpoint.NewEndpointWithTTL("new.foo.example.com", endpoint.RecordTypeA, 3600, "111.222.111.222"),
		endpoint.NewEndpointWithTTL("new.foo.example.com", endpoint.RecordTypeAAAA, 3600, "2001::111:222:111:222"),
		endpoint.NewEndpointWithTTL("newcname.foo.example.com", endpoint.RecordTypeCNAME, 10, "other.com"),
		endpoint.NewEndpointWithTTL("newns.foo.example.com", endpoint.RecordTypeNS, 10, "ns1.foo.example.com."),
		endpoint.NewEndpoint("new.nope.example.com", endpoint.RecordTypeA, "222.111.222.111"),
		endpoint.NewEndpoint("new.nope.example.com", endpoint.RecordTypeAAAA, "2001::222:111:222:111"),
		endpoint.NewEndpointWithTTL("newns.nope.example.com", endpoint.RecordTypeNS, 10, "ns1.nope.example.com."),
	}

	deleteRecords := []*endpoint.Endpoint{
		endpoint.NewEndpoint("deleted.foo.example.com", endpoint.RecordTypeA, "111.222.111.222"),
		endpoint.NewEndpoint("deletedaaaa.foo.example.com", endpoint.RecordTypeAAAA, "2001::111:222:111:222"),
		endpoint.NewEndpoint("deletedcname.foo.example.com", endpoint.RecordTypeCNAME, "other.com"),
		endpoint.NewEndpoint("deletedns.foo.example.com", endpoint.RecordTypeNS, "ns1.foo.example.com."),
		endpoint.NewEndpoint("deleted.nope.example.com", endpoint.RecordTypeA, "222.111.222.111"),
	}

	update, err := plan.MkUpdates(currentRecords, updatedRecords)
	assert.NoError(t, err)
	changes := &plan.Changes{
		Create: createRecords,
		Update: update,
		Delete: deleteRecords,
	}

	if err := provider.ApplyChanges(context.Background(), changes); err != nil {
		t.Fatal(err)
	}
}
