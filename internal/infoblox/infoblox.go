package infoblox

/*
Copyright 2024 The external-dns-infoblox-webhook Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

Generated by GoLic, for more details see: https://github.com/AbsaOSS/golic
*/

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"

	ibclient "github.com/infobloxopen/infoblox-go-client/v2"
	log "github.com/sirupsen/logrus"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/pkg/rfc2317"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
)

const (
	// provider specific key to track if PTR record was already created or not for A records
	providerSpecificInfobloxPtrRecord = "infoblox-ptr-record-exists"
	infobloxCreate                    = "CREATE"
	infobloxDelete                    = "DELETE"
	infobloxUpdate                    = "UPDATE"
)

func isNotFoundError(err error) bool {
	_, ok := err.(*ibclient.NotFoundError)
	return ok
}

type Provider struct {
	provider.BaseProvider
	client       ibclient.IBConnector
	domainFilter endpoint.DomainFilter
	config       *StartupConfig
}

// StartupConfig clarifies the method signature
type StartupConfig struct {
	Host         string `env:"INFOBLOX_HOST,required" envDefault:"localhost"`
	Port         int    `env:"INFOBLOX_PORT,required" envDefault:"443"`
	Username     string `env:"INFOBLOX_WAPI_USER,required"`
	Password     string `env:"INFOBLOX_WAPI_PASSWORD,required"`
	Version      string `env:"INFOBLOX_VERSION,required"`
	SSLVerify    bool   `env:"INFOBLOX_SSL_VERIFY" envDefault:"true"`
	DryRun       bool   `env:"INFOBLOX_DRY_RUN" envDefault:"false"`
	View         string `env:"INFOBLOX_VIEW" envDefault:"default"`
	MaxResults   int    `env:"INFOBLOX_MAX_RESULTS" envDefault:"1500"`
	CreatePTR    bool   `env:"INFOBLOX_CREATE_PTR" envDefault:"false"`
	DefaultTTL   int    `env:"INFOBLOX_DEFAULT_TTL" envDefault:"300"`
	ExtAttrsJSON string `env:"INFOBLOX_EXTENSIBLE_ATTRIBUTES_JSON" envDefault:"{}"`
	FQDNRegEx    string
	NameRegEx    string
}

type infobloxRecordSet struct {
	obj ibclient.IBObject
	res interface{}
}

// ExtendedRequestBuilder implements a HttpRequestBuilder which sets
// additional query parameter on all get requests
type ExtendedRequestBuilder struct {
	fqdnRegEx  string
	nameRegEx  string
	maxResults int
	ibclient.WapiRequestBuilder
}

// NewExtendedRequestBuilder returns a ExtendedRequestBuilder which adds
// _max_results query parameter to all GET requests
func NewExtendedRequestBuilder(maxResults int, fqdnRegEx string, nameRegEx string) *ExtendedRequestBuilder {
	return &ExtendedRequestBuilder{
		fqdnRegEx:  fqdnRegEx,
		nameRegEx:  nameRegEx,
		maxResults: maxResults,
	}
}

// BuildRequest prepares the api request. it uses BuildRequest of
// WapiRequestBuilder and then add the _max_requests parameter
func (mrb *ExtendedRequestBuilder) BuildRequest(t ibclient.RequestType, obj ibclient.IBObject, ref string, queryParams *ibclient.QueryParams) (req *http.Request, err error) {
	req, err = mrb.WapiRequestBuilder.BuildRequest(t, obj, ref, queryParams)
	if req.Method == "GET" {
		query := req.URL.Query()
		if mrb.maxResults > 0 {
			query.Set("_max_results", strconv.Itoa(mrb.maxResults))
		}
		_, zoneAuthQuery := obj.(*ibclient.ZoneAuth)
		if zoneAuthQuery && t == ibclient.GET && mrb.fqdnRegEx != "" {
			query.Set("fqdn~", mrb.fqdnRegEx)
		}

		// if we are not doing a ZoneAuth query, support the name filter
		if !zoneAuthQuery && mrb.nameRegEx != "" {
			query.Set("name~", mrb.nameRegEx)
		}

		req.URL.RawQuery = query.Encode()
	}
	return
}

// NewInfobloxProvider creates a new Infoblox provider.
func NewInfobloxProvider(cfg *StartupConfig, domainFilter endpoint.DomainFilter) (*Provider, error) {
	hostCfg := ibclient.HostConfig{
		Host:    cfg.Host,
		Port:    strconv.Itoa(cfg.Port),
		Version: cfg.Version,
	}

	authCfg := ibclient.AuthConfig{
		Username: cfg.Username,
		Password: cfg.Password,
	}

	httpPoolConnections := lookupEnvAtoi("EXTERNAL_DNS_INFOBLOX_HTTP_POOL_CONNECTIONS", 10)
	httpRequestTimeout := lookupEnvAtoi("EXTERNAL_DNS_INFOBLOX_HTTP_REQUEST_TIMEOUT", 60)

	transportConfig := ibclient.NewTransportConfig(
		strconv.FormatBool(cfg.SSLVerify),
		httpRequestTimeout,
		httpPoolConnections,
	)

	var (
		requestBuilder ibclient.HttpRequestBuilder
		err            error
	)
	if cfg.MaxResults != 0 || cfg.FQDNRegEx != "" || cfg.NameRegEx != "" {
		// use our own HttpRequestBuilder which sets _max_results parameter on GET requests
		requestBuilder = NewExtendedRequestBuilder(cfg.MaxResults, cfg.FQDNRegEx, cfg.NameRegEx)
	} else {
		// use the default HttpRequestBuilder of the infoblox client
		requestBuilder, err = ibclient.NewWapiRequestBuilder(hostCfg, authCfg)
		if err != nil {
			return nil, err
		}
	}

	requestor := &ibclient.WapiHttpRequestor{}

	client, err := ibclient.NewConnector(hostCfg, authCfg, transportConfig, requestBuilder, requestor)
	if err != nil {
		return nil, err
	}

	provider := &Provider{
		client:       client,
		domainFilter: domainFilter,
		config:       cfg,
	}

	return provider, nil
}

// Records gets the current records.
func (p *Provider) Records(_ context.Context) (endpoints []*endpoint.Endpoint, err error) {
	zones, err := p.zones()
	if err != nil {
		return nil, fmt.Errorf("could not fetch zones: %w", err)
	}
	extAttrs, err := deserializeEAs(p.config.ExtAttrsJSON)
	if err != nil {
		return nil, err
	}

	for _, zone := range zones {
		log.Debugf("fetch records from zone '%s'", zone.Fqdn)
		searchParams := map[string]string{"zone": zone.Fqdn, "view": p.config.View}
		var resA []ibclient.RecordA
		objA := ibclient.NewEmptyRecordA()
		objA.View = p.config.View
		objA.Ea = extAttrs
		objA.Zone = zone.Fqdn
		err = PagingGetObject(p.client, objA, "", searchParams, &resA)
		if err != nil && !isNotFoundError(err) {
			return nil, fmt.Errorf("could not fetch A records from zone '%s': %w", zone.Fqdn, err)
		}
		endpointsA := ToAResponseMap(resA).ToEndpoints()
		endpoints = append(endpoints, endpointsA...)

		// Include Host records since they should be treated synonymously with A records
		var resH []ibclient.HostRecord
		objH := ibclient.NewEmptyHostRecord()
		objH.View = &p.config.View
		objH.Ea = extAttrs
		objH.Zone = zone.Fqdn
		err = PagingGetObject(p.client, objH, "", searchParams, &resH)
		if err != nil && !isNotFoundError(err) {
			return nil, fmt.Errorf("could not fetch host records from zone '%s': %w", zone.Fqdn, err)
		}
		endpointsHost := ToHostResponseMap(resH).ToEndpoints()
		endpoints = append(endpoints, endpointsHost...)

		var resC []ibclient.RecordCNAME
		objC := ibclient.NewEmptyRecordCNAME()
		objC.View = &p.config.View
		objC.Ea = extAttrs
		objC.Zone = zone.Fqdn
		err = PagingGetObject(p.client, objC, "", searchParams, &resC)
		if err != nil && !isNotFoundError(err) {
			return nil, fmt.Errorf("could not fetch CNAME records from zone '%s': %w", zone.Fqdn, err)
		}
		endpointsCNAME := ToCNAMEResponseMap(resC).ToEndpoints()
		endpoints = append(endpoints, endpointsCNAME...)

		var resT []ibclient.RecordTXT
		objT := ibclient.NewEmptyRecordTXT()
		objT.View = &p.config.View
		objT.Ea = extAttrs
		objT.Zone = zone.Fqdn
		err = PagingGetObject(p.client, objT, "", searchParams, &resT)
		if err != nil && !isNotFoundError(err) {
			return nil, fmt.Errorf("could not fetch TXT records from zone '%s': %w", zone.Fqdn, err)
		}
		endpointsTXT := ToTXTResponseMap(resT).ToEndpoints()
		endpoints = append(endpoints, endpointsTXT...)

		if p.config.CreatePTR {
			arpaZone, err := rfc2317.CidrToInAddr(zone.Fqdn)
			if err == nil {
				var resP []ibclient.RecordPTR
				objP := ibclient.NewEmptyRecordPTR()
				objP.View = p.config.View
				objP.Ea = extAttrs
				objP.Zone = arpaZone
				err = PagingGetObject(p.client, objP, "", map[string]string{"zone": arpaZone, "view": p.config.View}, &resP)
				if err != nil && !isNotFoundError(err) {
					return nil, fmt.Errorf("could not fetch PTR records from zone '%s': %w", zone.Fqdn, err)
				}
				endpointsPTR := ToPTRResponseMap(resP).ToEndpoints()
				endpoints = append(endpoints, endpointsPTR...)
			} else {
				log.Debugf("Could not fetch PTR records from zone '%s': %s", zone.Fqdn, err)
			}
		}
	}

	if p.config.CreatePTR {
		// save all ptr records into map for a quick look up
		ptrRecordsMap := make(map[string]bool)
		for _, ptrRecord := range endpoints {
			if ptrRecord.RecordType != endpoint.RecordTypePTR {
				continue
			}
			ptrRecordsMap[ptrRecord.DNSName] = true
		}

		for i := range endpoints {
			if endpoints[i].RecordType != endpoint.RecordTypeA {
				continue
			}
			// if PTR record already exists for A record, then mark it as such
			if ptrRecordsMap[endpoints[i].DNSName] {
				found := false
				for j := range endpoints[i].ProviderSpecific {
					if endpoints[i].ProviderSpecific[j].Name == providerSpecificInfobloxPtrRecord {
						endpoints[i].ProviderSpecific[j].Value = "true"
						found = true
					}
				}
				if !found {
					endpoints[i].WithProviderSpecific(providerSpecificInfobloxPtrRecord, "true")
				}
			}
		}
	}

	log.Debugf("fetched %d records from infoblox", len(endpoints))
	return endpoints, nil
}

func (p *Provider) AdjustEndpoints(endpoints []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) {
	// Update user specified TTL (0 == disabled)
	for _, ep := range endpoints {
		if !ep.RecordTTL.IsConfigured() {
			ep.RecordTTL = endpoint.TTL(p.config.DefaultTTL)
		}
	}

	if !p.config.CreatePTR {
		return endpoints, nil
	}

	// for all A records, we want to create PTR records
	// so add provider specific property to track if the record was created or not
	for i := range endpoints {
		if endpoints[i].RecordType == endpoint.RecordTypeA {
			found := false
			for j := range endpoints[i].ProviderSpecific {
				if endpoints[i].ProviderSpecific[j].Name == providerSpecificInfobloxPtrRecord {
					endpoints[i].ProviderSpecific[j].Value = "true"
					found = true
				}
			}
			if !found {
				endpoints[i].WithProviderSpecific(providerSpecificInfobloxPtrRecord, "true")
			}
		}
	}

	return endpoints, nil
}

func newIBChanges(action string, eps []*endpoint.Endpoint) []*infobloxChange {
	changes := make([]*infobloxChange, 0, len(eps))
	for _, ep := range eps {
		for _, target := range ep.Targets {
			newEp := ep.DeepCopy()
			newEp.Targets = endpoint.Targets{target}
			changes = append(changes, &infobloxChange{
				Action:   action,
				Endpoint: newEp,
			})
		}
	}

	return changes
}

func zonePointerConverter(in []ibclient.ZoneAuth) []*ibclient.ZoneAuth {
	out := make([]*ibclient.ZoneAuth, len(in))
	for i := range in {
		out[i] = &in[i]
	}
	return out
}

// submitChanges sends changes to Infoblox
func (p *Provider) submitChanges(changes []*infobloxChange) error {
	// return early if there is nothing to change
	if len(changes) == 0 {
		return nil
	}

	zones, err := p.zones()
	if err != nil {
		return fmt.Errorf("could not fetch zones: %w", err)
	}

	changesByZone := p.ChangesByZone(zonePointerConverter(zones), changes)
	for _, changes := range changesByZone {
		for _, change := range changes {
			record, err := p.buildRecord(change)
			if err != nil {
				return fmt.Errorf("could not build record: %w", err)
			}
			refId, logFields, err := getRefID(record)
			if err != nil {
				return err
			}
			logFields["action"] = change.Action
			if p.config.DryRun {
				log.WithFields(logFields).Info("Dry run: skipping..")
				continue
			}
			log.WithFields(logFields).Info("Changing record")
			switch change.Action {
			case infobloxCreate:
				_, err = p.client.CreateObject(record.obj)
				if err != nil {
					return err
				}
			case infobloxDelete:
				_, err = p.client.DeleteObject(refId)
				if err != nil {
					return err
				}
			case infobloxUpdate:
				_, err = p.client.UpdateObject(record.obj, refId)
				if err != nil {
					return err
				}
			default:
				return fmt.Errorf("unknown action '%s'", change.Action)
			}
		}
	}

	return nil
}

func getRefID(record *infobloxRecordSet) (string, log.Fields, error) {
	t := reflect.TypeOf(record.obj).Elem().Name()
	l := log.Fields{
		"type": t,
	}
	switch t {
	case "RecordA":
		l["record"] = AsString(record.obj.(*ibclient.RecordA).Name)
		l["ttl"] = AsInt64(record.obj.(*ibclient.RecordA).Ttl)
		l["target"] = AsString(record.obj.(*ibclient.RecordA).Ipv4Addr)
		for _, r := range *record.res.(*[]ibclient.RecordA) {
			return r.Ref, l, nil
		}
		return "", l, nil
	case "RecordTXT":
		l["record"] = AsString(record.obj.(*ibclient.RecordTXT).Name)
		l["ttl"] = AsInt64(record.obj.(*ibclient.RecordTXT).Ttl)
		l["target"] = AsString(record.obj.(*ibclient.RecordTXT).Text)
		for _, r := range *record.res.(*[]ibclient.RecordTXT) {
			return r.Ref, l, nil
		}
		return "", l, nil
	case "RecordCNAME":
		l["record"] = AsString(record.obj.(*ibclient.RecordCNAME).Name)
		l["ttl"] = AsInt64(record.obj.(*ibclient.RecordCNAME).Ttl)
		l["target"] = AsString(record.obj.(*ibclient.RecordCNAME).Canonical)
		for _, r := range *record.res.(*[]ibclient.RecordCNAME) {
			return r.Ref, l, nil
		}
		return "", l, nil
	case "RecordPTR":
		l["record"] = AsString(record.obj.(*ibclient.RecordPTR).Name)
		l["ttl"] = AsInt64(record.obj.(*ibclient.RecordPTR).Ttl)
		l["target"] = AsString(record.obj.(*ibclient.RecordPTR).PtrdName)
		for _, r := range *record.res.(*[]ibclient.RecordPTR) {
			return r.Ref, l, nil
		}
		return "", l, nil
	}
	return "", l, fmt.Errorf("unknown type '%s'", t)
}

// if updateNew is not part of Update Old , object should be created
// if updateOld is not part of Update New , object should be deleted
// if it is not there (TTL might change) , object should be updated
// if we rename the object , object should be deleted and created
func (p *Provider) CountDiff(changes *plan.Changes) {

	endpointsToMap := func(eps []*endpoint.Endpoint) map[string]*endpoint.Endpoint {
		m := map[string]*endpoint.Endpoint{}
		for _, v := range eps {
			m[v.DNSName+"_"+v.RecordType] = v
		}
		return m
	}

	targetsToMap := func(targets endpoint.Targets) map[string]bool {
		m := map[string]bool{}
		for _, v := range targets {
			m[v] = true
		}
		return m
	}

	cloneWithSingleTarget := func(ep *endpoint.Endpoint, target string) *endpoint.Endpoint {
		clone := ep.DeepCopy()
		clone.Targets = endpoint.Targets{target}
		return clone
	}

	removeTargetFromEndpoint := func(ep *endpoint.Endpoint, target string) {
		for i, t := range ep.Targets {
			if t == target {
				ep.Targets = append(ep.Targets[:i], ep.Targets[i+1:]...)
				break
			}
		}
	}

	removeFromEndpointSlice := func(eps []*endpoint.Endpoint, ep *endpoint.Endpoint) []*endpoint.Endpoint {
		for i, e := range eps {
			if e.DNSName == ep.DNSName {
				return append(eps[:i], eps[i+1:]...)
			}
		}
		return eps

	}

	updateNewMap := endpointsToMap(changes.UpdateNew)
	updateOldMap := endpointsToMap(changes.UpdateOld)

	for k, oldEp := range updateOldMap {
		newEp := updateNewMap[k]

		if newEp == nil {
			changes.Delete = append(changes.Delete, oldEp)
			changes.UpdateOld = removeFromEndpointSlice(changes.UpdateOld, oldEp)
			delete(updateOldMap, k)
		}
	}

	// TODO: consider if old and new can be different. If yes, then we need to handle that case
	for k, newEp := range updateNewMap {
		oldEp := updateOldMap[k]

		//exists in updateNew bud doesnt exists in updateOld, than create
		if oldEp == nil {
			changes.Create = append(changes.Create, newEp)
			changes.UpdateNew = removeFromEndpointSlice(changes.UpdateNew, newEp)
			continue
		}

		// oldEP is found
		oldTargets := targetsToMap(oldEp.Targets)
		newTargets := targetsToMap(newEp.Targets)

		for target := range oldTargets {
			if !newTargets[target] {
				// delete
				changes.Delete = append(changes.Delete, cloneWithSingleTarget(oldEp, target))
				removeTargetFromEndpoint(newEp, target)
			}
		}

		for target := range newTargets {
			if !oldTargets[target] {
				// create
				changes.Create = append(changes.Create, cloneWithSingleTarget(newEp, target))
				removeTargetFromEndpoint(newEp, target)
			}
		}

		//for target, _ := range newTargets {
		//	if oldTargets[target] {
		//		// update
		//		changes.UpdateNew = append(changes.UpdateNew, cloneWithSingleTarget(newEp, target))
		//	}
		//}
	}
}

// ApplyChanges applies the given changes.
func (p *Provider) ApplyChanges(_ context.Context, changes *plan.Changes) error {

	p.CountDiff(changes)

	combinedChanges := make([]*infobloxChange, 0, len(changes.Create)+len(changes.UpdateNew)+len(changes.Delete))

	combinedChanges = append(combinedChanges, newIBChanges(infobloxCreate, changes.Create)...)
	combinedChanges = append(combinedChanges, newIBChanges(infobloxUpdate, changes.UpdateNew)...)
	combinedChanges = append(combinedChanges, newIBChanges(infobloxDelete, changes.Delete)...)

	return p.submitChanges(combinedChanges)
}

func (p *Provider) zones() ([]ibclient.ZoneAuth, error) {
	var res, result []ibclient.ZoneAuth
	obj := ibclient.NewZoneAuth(
		ibclient.ZoneAuth{
			View: &p.config.View,
		},
	)
	searchFields := map[string]string{}
	if p.config.View != "" {
		searchFields["view"] = p.config.View
	}
	err := PagingGetObject(p.client, obj, "", searchFields, &res)
	if err != nil && !isNotFoundError(err) {
		return nil, err
	}

	for _, zone := range res {
		if !p.domainFilter.Match(zone.Fqdn) {
			continue
		}

		//
		//if !p.config.ZoneIDFilter.Match(zone.Ref) {
		//	continue
		//}

		result = append(result, zone)
	}

	return result, nil
}

type infobloxChange struct {
	Action   string
	Endpoint *endpoint.Endpoint
}

func (p *Provider) ChangesByZone(zones []*ibclient.ZoneAuth, changeSets []*infobloxChange) map[string][]*infobloxChange {
	changes := make(map[string][]*infobloxChange)
	for _, z := range zones {
		changes[z.Fqdn] = []*infobloxChange{}
	}

	for _, c := range changeSets {
		zone := p.findZone(zones, c.Endpoint.DNSName)
		if zone == nil || zone.Fqdn == "" {
			log.Debugf("Skipping record %s because no hosted zone matching record DNS Name was detected", c.Endpoint.DNSName)
			continue
		}
		changes[zone.Fqdn] = append(changes[zone.Fqdn], c)

		if p.config.CreatePTR && c.Endpoint.RecordType == endpoint.RecordTypeA {
			reverseZone := p.findReverseZone(zones, c.Endpoint.Targets[0])
			if reverseZone == nil {
				log.Debugf("Ignoring changes to '%s' because a suitable Infoblox DNS reverse zone was not found.", c.Endpoint.Targets)
				continue
			}
			copyEp := *c.Endpoint
			copyEp.RecordType = endpoint.RecordTypePTR
			changes[reverseZone.Fqdn] = append(changes[reverseZone.Fqdn], &infobloxChange{c.Action, &copyEp})
		}
	}
	return changes
}

func (p *Provider) findZone(zones []*ibclient.ZoneAuth, name string) *ibclient.ZoneAuth {
	var result *ibclient.ZoneAuth

	// Go through every zone looking for the longest name (i.e. most specific) as a matching suffix
	for idx := range zones {
		zone := zones[idx]
		if strings.HasSuffix(name, "."+zone.Fqdn) {
			if result == nil || len(zone.Fqdn) > len(result.Fqdn) {
				result = zone
			}
		} else if strings.EqualFold(name, zone.Fqdn) {
			if result == nil || len(zone.Fqdn) > len(result.Fqdn) {
				result = zone
			}
		}
	}
	return result
}

func (p *Provider) findReverseZone(zones []*ibclient.ZoneAuth, name string) *ibclient.ZoneAuth {
	ip := net.ParseIP(name)
	networks := map[int]*ibclient.ZoneAuth{}
	maxMask := 0

	for i, zone := range zones {
		_, rZoneNet, err := net.ParseCIDR(zone.Fqdn)
		if err != nil {
			log.WithError(err).Debugf("fqdn %s is no cidr", zone.Fqdn)
		} else {
			if rZoneNet.Contains(ip) {
				_, mask := rZoneNet.Mask.Size()
				networks[mask] = zones[i]
				if mask > maxMask {
					maxMask = mask
				}
			}
		}
	}
	return networks[maxMask]
}

func (p *Provider) recordSet(ep *endpoint.Endpoint, getObject bool) (recordSet infobloxRecordSet, err error) {
	var ttl uint32
	if ep.RecordTTL.IsConfigured() {
		ttl = uint32(ep.RecordTTL)
	}
	extAttrs, err := deserializeEAs(p.config.ExtAttrsJSON)
	if err != nil {
		return
	}
	ptrToBoolTrue := true
	switch ep.RecordType {
	case endpoint.RecordTypeA:
		var res []ibclient.RecordA
		obj := ibclient.NewEmptyRecordA()
		obj.Name = &ep.DNSName
		// TODO: get target index
		obj.Ipv4Addr = &ep.Targets[0]
		obj.Ea = extAttrs
		obj.Ttl = &ttl
		obj.UseTtl = &ptrToBoolTrue
		if getObject {
			queryParams := ibclient.NewQueryParams(false, map[string]string{"name": *obj.Name, "ipv4addr": *obj.Ipv4Addr})
			err = p.client.GetObject(obj, "", queryParams, &res)
			if err != nil && !isNotFoundError(err) {
				err = fmt.Errorf("could not fetch A record ['%s':'%s'] : %w", *obj.Name, *obj.Ipv4Addr, err)
				return
			}
		} else {
			// If getObject is not set (action == create), we need to set the View for Infoblox to find the parent zone
			// If View is set for the other actions, Infoblox will complain that the view field is not allowed
			obj.View = p.config.View
		}
		recordSet = infobloxRecordSet{
			obj: obj,
			res: &res,
		}
	case endpoint.RecordTypePTR:
		var res []ibclient.RecordPTR
		obj := ibclient.NewEmptyRecordPTR()
		obj.PtrdName = &ep.DNSName
		// TODO: get target index
		obj.Ipv4Addr = &ep.Targets[0]
		obj.Ea = extAttrs
		obj.Ttl = &ttl
		obj.UseTtl = &ptrToBoolTrue
		if getObject {
			queryParams := ibclient.NewQueryParams(false, map[string]string{"name": *obj.PtrdName})
			err = p.client.GetObject(obj, "", queryParams, &res)
			if err != nil && !isNotFoundError(err) {
				return
			}
		} else {
			// If getObject is not set (action == create), we need to set the View for Infoblox to find the parent zone
			// If View is set for the other actions, Infoblox will complain that the view field is not allowed
			obj.View = p.config.View
		}
		recordSet = infobloxRecordSet{
			obj: obj,
			res: &res,
		}
	case endpoint.RecordTypeCNAME:
		var res []ibclient.RecordCNAME
		obj := ibclient.NewEmptyRecordCNAME()
		obj.Name = &ep.DNSName
		obj.Canonical = &ep.Targets[0]
		obj.Ea = extAttrs
		obj.Ttl = &ttl
		obj.UseTtl = &ptrToBoolTrue
		if getObject {
			queryParams := ibclient.NewQueryParams(false, map[string]string{"name": *obj.Name})
			err = p.client.GetObject(obj, "", queryParams, &res)
			if err != nil && !isNotFoundError(err) {
				return
			}
		} else {
			// If getObject is not set (action == create), we need to set the View for Infoblox to find the parent zone
			// If View is set for the other actions, Infoblox will complain that the view field is not allowed
			obj.View = &p.config.View
		}
		recordSet = infobloxRecordSet{
			obj: obj,
			res: &res,
		}
	case endpoint.RecordTypeTXT:
		var res []ibclient.RecordTXT
		// The Infoblox API strips enclosing double quotes from TXT records lacking whitespace.
		// Here we reconcile that fact by making this state match that reality.
		if target, err2 := strconv.Unquote(ep.Targets[0]); err2 == nil && !strings.Contains(ep.Targets[0], " ") {
			ep.Targets = endpoint.Targets{target}
		}
		obj := ibclient.NewEmptyRecordTXT()
		obj.Text = &ep.Targets[0]
		obj.Name = &ep.DNSName
		obj.Ea = extAttrs
		obj.Ttl = &ttl
		obj.UseTtl = &ptrToBoolTrue
		// TODO: Zone?
		if getObject {
			queryParams := ibclient.NewQueryParams(false, map[string]string{"name": *obj.Name})
			err = p.client.GetObject(obj, "", queryParams, &res)
			if err != nil && !isNotFoundError(err) {
				return
			}
		} else {
			// If getObject is not set (action == create), we need to set the View for Infoblox to find the parent zone
			// If View is set for the other actions, Infoblox will complain that the view field is not allowed
			obj.View = &p.config.View
		}
		recordSet = infobloxRecordSet{
			obj: obj,
			res: &res,
		}
	}
	return
}

func (p *Provider) buildRecord(change *infobloxChange) (*infobloxRecordSet, error) {
	rs, err := p.recordSet(change.Endpoint, !(change.Action == infobloxCreate))
	if err != nil {
		return nil, err
	}
	return &rs, nil
}

func lookupEnvAtoi(key string, fallback int) (i int) {
	val, ok := os.LookupEnv(key)
	if !ok {
		i = fallback
		return
	}
	i, err := strconv.Atoi(val)
	if err != nil {
		i = fallback
		return
	}
	return
}

func deserializeEAs(extAttrJSON string) (map[string]interface{}, error) {
	extAttrs := make(map[string]interface{})
	if extAttrJSON == "" {
		return extAttrs, nil
	}
	if err := json.Unmarshal([]byte(extAttrJSON), &extAttrs); err != nil {
		return nil, fmt.Errorf("cannot process 'ext_attrs' field: %w", err)
	}
	return extAttrs, nil
}
