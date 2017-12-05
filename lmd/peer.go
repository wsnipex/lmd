package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a8m/djson"
	"github.com/buger/jsonparser"
)

var reResponseHeader = regexp.MustCompile(`^(\d+)\s+(\d+)$`)
var reHTTPTooOld = regexp.MustCompile(`Can.t locate object method`)
var reHTTPOMDError = regexp.MustCompile(`<h1>(OMD:.*?)</h1>`)
var reShinkenVersion = regexp.MustCompile(`-shinken$`)
var reIcinga2Version = regexp.MustCompile(`^r[\d\.-]+$`)

const (
	// UpdateAdditionalDelta is the number of seconds to add to the last_check filter on delta updates
	UpdateAdditionalDelta = 3

	// MinFullScanInterval is the minimum interval between two full scans
	MinFullScanInterval = 30
)

// DataTable contains the actual data with a reference to the table.
type DataTable struct {
	Table      *Table
	Data       [][]interface{}
	Refs       map[string][][]interface{}
	Index      map[string][]interface{}
	LastUpdate []int64
}

// Peer is the object which handles collecting and updating data and connections.
type Peer struct {
	Name            string
	ID              string
	Source          []string
	PeerLock        *LoggingLock // must be used for Peer.Status access
	DataLock        *LoggingLock // must be used for Peer.Table access
	Tables          map[string]DataTable
	Status          map[string]interface{}
	ErrorCount      int
	ErrorLogged     bool
	waitGroup       *sync.WaitGroup
	shutdownChannel chan bool
	stopChannel     chan bool
	Config          *Connection
	Flags           OptionalFlags
	LocalConfig     *Config
	lastRequest     *Request
	lastResponse    *[]byte
}

// PeerStatus contains the different states a peer can have
type PeerStatus int

// A peer can be up, warning, down and pending.
// It is pending right after start and warning when the connection fails
// but the stale timeout is not yet hit.
const (
	PeerStatusUp PeerStatus = iota
	PeerStatusWarning
	PeerStatusDown
	PeerStatusBroken // broken flags clients which cannot be used, lmd will check program_start and only retry them if start time changed
	PeerStatusPending
)

// PeerErrorType is used to distinguish between connection and response errors.
type PeerErrorType int

const (
	// ConnectionError is used when the connection to a remote site failed
	ConnectionError PeerErrorType = iota

	// ResponseError is used when the remote site is available but returns an unusable result.
	ResponseError
)

// HTTPResult contains the livestatus result as long with some meta data.
type HTTPResult struct {
	Rc      int
	Version string
	Branch  string
	Output  json.RawMessage
}

// PeerError is a custom error to distinguish between connection and response errors.
type PeerError struct {
	msg      string
	kind     PeerErrorType
	req      *Request
	res      [][]interface{}
	resBytes *[]byte
}

// Error returns the error message as string.
func (e *PeerError) Error() string {
	msg := e.msg
	if e.req != nil {
		msg += fmt.Sprintf("\nRequest: %s", e.req.String())
	}
	if e.res != nil {
		msg += fmt.Sprintf("\nResponse: %#v", e.res)
	}
	if e.resBytes != nil {
		msg += fmt.Sprintf("\nResponse: %s", string(*(e.resBytes)))
	}
	return msg
}

// Type returns the error type.
func (e *PeerError) Type() PeerErrorType { return e.kind }

// AddItem adds an new entry to a datatable.
func (d *DataTable) AddItem(row *[]interface{}) {
	d.Data = append(d.Data, *row)
}

// RemoveItem removes an entry from a datatable.
func (d *DataTable) RemoveItem(row []interface{}) {
	for i := range d.Data {
		r := d.Data[i]
		if fmt.Sprintf("%p", r) == fmt.Sprintf("%p", row) {
			d.Data = append(d.Data[:i], d.Data[i+1:]...)
			delete(d.Index, fmt.Sprintf("%v", r[d.Table.GetColumn("id").Index]))
			return
		}
	}
	log.Panicf("element not found")
}

// NewPeer creates a new peer object.
// It returns the created peer.
func NewPeer(LocalConfig *Config, config *Connection, waitGroup *sync.WaitGroup, shutdownChannel chan bool) *Peer {
	p := Peer{
		Name:            config.Name,
		ID:              config.ID,
		Source:          config.Source,
		Tables:          make(map[string]DataTable),
		Status:          make(map[string]interface{}),
		ErrorCount:      0,
		waitGroup:       waitGroup,
		shutdownChannel: shutdownChannel,
		stopChannel:     make(chan bool),
		PeerLock:        NewLoggingLock(config.Name + "PeerLock"),
		DataLock:        NewLoggingLock(config.Name + "DataLock"),
		Config:          config,
		LocalConfig:     LocalConfig,
	}
	p.Status["PeerKey"] = p.ID
	p.Status["PeerName"] = p.Name
	p.Status["CurPeerAddrNum"] = 0
	p.Status["PeerAddr"] = p.Source[p.Status["CurPeerAddrNum"].(int)]
	p.Status["PeerStatus"] = PeerStatusPending
	p.Status["LastUpdate"] = int64(0)
	p.Status["LastFullUpdate"] = int64(0)
	p.Status["LastFullHostUpdate"] = int64(0)
	p.Status["LastFullServiceUpdate"] = int64(0)
	p.Status["LastQuery"] = int64(0)
	p.Status["LastError"] = "connecting..."
	p.Status["LastOnline"] = int64(0)
	p.Status["ProgramStart"] = 0
	p.Status["BytesSend"] = 0
	p.Status["BytesReceived"] = 0
	p.Status["Querys"] = 0
	p.Status["ReponseTime"] = 0
	p.Status["Idling"] = false
	p.Status["Updating"] = false

	/* strip of trailing slashes from http backends */
	for i, s := range p.Source {
		for len(s) > 0 && s[len(s)-1] == '/' {
			s = s[0 : len(s)-1]
		}
		p.Source[i] = s
	}

	return &p
}

// Start creates the initial objects and starts the update loop in a separate goroutine.
func (p *Peer) Start() {
	if p.StatusGet("Updating").(bool) {
		log.Panicf("[%s] tried to start updateLoop twice", p.Name)
	}
	p.waitGroup.Add(1)
	p.StatusSet("Updating", true)
	log.Infof("[%s] starting connection", p.Name)
	go func() {
		// make sure we log panics properly
		defer logPanicExitPeer(p)
		p.updateLoop()
		p.StatusSet("Updating", false)
		p.PeerLock.Lock()
		p.waitGroup.Done()
		p.PeerLock.Unlock()
	}()
}

// Stop stops this peer. Restart with Start.
func (p *Peer) Stop() {
	if p.StatusGet("Updating").(bool) {
		log.Infof("[%s] stopping connection", p.Name)
		p.stopChannel <- true
	}
}

func (p *Peer) countFromServer(name string, queryCondition string) (count int) {
	count = -1
	res, err := p.QueryString("GET " + name + "\nOutputFormat: json\nStats: " + queryCondition + "\n\n")
	if err == nil && len(res) > 0 && len(res[0]) > 0 {
		count = int(res[0][0].(float64))
	}
	return
}

func (p *Peer) hasChanged() (changed bool) {
	changed = false
	tablenames := []string{"commands", "contactgroups", "contacts", "hostgroups", "hosts", "servicegroups", "timeperiods"}
	for _, name := range tablenames {
		counter := p.countFromServer(name, "name !=")
		p.DataLock.RLock()
		changed = changed || (counter != len(p.Tables[name].Data))
		p.DataLock.RUnlock()
	}
	counter := p.countFromServer("services", "host_name !=")
	p.DataLock.RLock()
	changed = changed || (counter != len(p.Tables["services"].Data))
	p.DataLock.RUnlock()
	p.clearLastRequest()

	return
}

// Clear resets the data table.
func (p *Peer) Clear() {
	p.Tables = make(map[string]DataTable)
}

// updateLoop is the main loop updating this peer.
// It does not return till triggered by the shutdownChannel or by the internal stopChannel.
func (p *Peer) updateLoop() {
	var ok bool
	var lastTimeperiodUpdateMinute int
	p.PeerLock.RLock()
	firstRun := true
	if value, gotLastValue := p.Status["LastUpdateOK"]; gotLastValue {
		ok = value.(bool)
		lastTimeperiodUpdateMinute = p.Status["LastTimeperiodUpdateMinute"].(int)
		firstRun = false
	}
	p.PeerLock.RUnlock()

	// First run, initialize tables
	if firstRun {
		ok = p.InitAllTables()
		lastTimeperiodUpdateMinute, _ = strconv.Atoi(time.Now().Format("4"))
		p.PeerLock.Lock()
		p.Status["LastUpdateOK"] = ok
		p.Status["LastTimeperiodUpdateMinute"] = lastTimeperiodUpdateMinute
		p.PeerLock.Unlock()
		p.clearLastRequest()
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	for {
		select {
		case <-p.shutdownChannel:
			log.Debugf("[%s] stopping...", p.Name)
			ticker.Stop()
			return
		case <-p.stopChannel:
			log.Debugf("[%s] stopping...", p.Name)
			ticker.Stop()
			p.PeerLock.Lock()
			p.Status["LastUpdateOK"] = ok
			p.Status["LastTimeperiodUpdateMinute"] = lastTimeperiodUpdateMinute
			p.PeerLock.Unlock()
			p.clearLastRequest()
			return
		case <-ticker.C:
			p.periodicUpdate(&ok, &lastTimeperiodUpdateMinute)
			p.clearLastRequest()
		}
	}
}

// periodicUpdate runs the periodic updates from the update loop
func (p *Peer) periodicUpdate(ok *bool, lastTimeperiodUpdateMinute *int) {
	p.PeerLock.RLock()
	lastUpdate := p.Status["LastUpdate"].(int64)
	lastFullUpdate := p.Status["LastFullUpdate"].(int64)
	lastStatus := p.Status["PeerStatus"].(PeerStatus)
	p.PeerLock.RUnlock()

	idling := p.updateIdleStatus()
	now := time.Now().Unix()
	currentMinute, _ := strconv.Atoi(time.Now().Format("4"))
	if idling {
		if now < lastUpdate+p.LocalConfig.IdleInterval {
			return
		}
	} else {
		// update timeperiods every full minute except when idling
		if *ok && *lastTimeperiodUpdateMinute != currentMinute && lastStatus != PeerStatusBroken {
			log.Debugf("[%s] updating timeperiods and host/servicegroup statistics", p.Name)
			p.UpdateObjectByType(Objects.Tables["timeperiods"])
			p.UpdateObjectByType(Objects.Tables["hostgroups"])
			p.UpdateObjectByType(Objects.Tables["servicegroups"])
			*lastTimeperiodUpdateMinute = currentMinute

			*ok = p.checkIcinga2Reload()
		}

		if now < lastUpdate+p.LocalConfig.Updateinterval {
			return
		}
	}

	// set last update timestamp, otherwise we would retry the connection every 500ms instead
	// of the update interval
	p.StatusSet("LastUpdate", time.Now().Unix())

	if lastStatus == PeerStatusBroken {
		restartRequired, _ := p.UpdateObjectByType(Objects.Tables["status"])
		if restartRequired {
			log.Debugf("[%s] broken peer has reloaded, trying again.", p.Name)
			*ok = p.InitAllTables()
			*lastTimeperiodUpdateMinute = currentMinute
		} else {
			log.Debugf("[%s] waiting for reload", p.Name)
		}
		return
	}

	// run full update if the site was down.
	// run update if it was just a short outage
	if !*ok && lastStatus != PeerStatusWarning {
		*ok = p.InitAllTables()
		*lastTimeperiodUpdateMinute = currentMinute
		return
	}

	// full update interval
	if !idling && p.LocalConfig.FullUpdateInterval > 0 && now > lastFullUpdate+p.LocalConfig.FullUpdateInterval {
		*ok = p.UpdateAllTables()
		return
	}

	*ok = p.UpdateDeltaTables()
}

func (p *Peer) updateIdleStatus() bool {
	now := time.Now().Unix()
	shouldIdle := false
	p.PeerLock.RLock()
	lastQuery := p.Status["LastQuery"].(int64)
	idling := p.Status["Idling"].(bool)
	p.PeerLock.RUnlock()
	if lastQuery == 0 && lastMainRestart < now-p.LocalConfig.IdleTimeout {
		shouldIdle = true
	} else if lastQuery > 0 && lastQuery < now-p.LocalConfig.IdleTimeout {
		shouldIdle = true
	}
	if !idling && shouldIdle {
		log.Infof("[%s] switched to idle interval, last query: %s", p.Name, timeOrNever(lastQuery))
		p.StatusSet("Idling", true)
		idling = true
	}
	return idling
}

// StatusSet updates a status map and takes care about the logging.
func (p *Peer) StatusSet(key string, value interface{}) {
	p.PeerLock.LockN(2)
	p.Status[key] = value
	p.PeerLock.Unlock()
}

// StatusGet returns a status map entry and takes care about the logging.
func (p *Peer) StatusGet(key string) interface{} {
	p.PeerLock.RLockN(2)
	value := p.Status[key]
	p.PeerLock.RUnlock()
	return value
}

// ScheduleImmediateUpdate resets all update timer so the next updateloop iteration
// will performan an update.
func (p *Peer) ScheduleImmediateUpdate() {
	p.StatusSet("LastUpdate", time.Now().Unix()-p.LocalConfig.Updateinterval-1)
	p.StatusSet("LastFullServiceUpdate", time.Now().Unix()-MinFullScanInterval-1)
	p.StatusSet("LastFullHostUpdate", time.Now().Unix()-MinFullScanInterval-1)
}

// InitAllTables creates all tables for this peer.
// It returns true if the import was successful or false otherwise.
func (p *Peer) InitAllTables() bool {
	var err error
	p.PeerLock.Lock()
	p.Status["LastUpdate"] = time.Now().Unix()
	p.Status["LastFullUpdate"] = time.Now().Unix()
	p.PeerLock.Unlock()
	t1 := time.Now()
	for _, n := range Objects.Order {
		t := Objects.Tables[n]
		_, err = p.CreateObjectByType(t)
		if err != nil {
			log.Debugf("[%s] creating initial objects failed in table %s: %s", p.Name, t.Name, err.Error())
			return false
		}
	}
	// this may happen if we query another lmd daemon which has no backends ready yet
	p.DataLock.RLock()
	hasStatus := len(p.Tables["status"].Data) > 0
	p.DataLock.RUnlock()
	if !hasStatus {
		p.PeerLock.Lock()
		p.Status["PeerStatus"] = PeerStatusWarning
		p.Status["LastError"] = "peered partner not ready yet"
		p.PeerLock.Unlock()
		return false
	}

	p.DataLock.RLock()
	programStart := p.Tables["status"].Data[0][p.Tables["status"].Table.ColumnsIndex["program_start"]]
	p.DataLock.RUnlock()

	duration := time.Since(t1)
	p.PeerLock.Lock()
	p.Status["ProgramStart"] = programStart
	p.Status["ReponseTime"] = duration.Seconds()
	p.PeerLock.Unlock()
	log.Infof("[%s] objects created in: %s", p.Name, duration.String())

	peerStatus := p.StatusGet("PeerStatus").(PeerStatus)
	if peerStatus != PeerStatusUp {
		// Reset errors
		if peerStatus == PeerStatusDown {
			log.Infof("[%s] site is back online", p.Name)
		}
		p.resetErrors()
	}
	promPeerUpdates.WithLabelValues(p.Name).Inc()
	promPeerUpdateDuration.WithLabelValues(p.Name).Set(duration.Seconds())

	return true
}

// resetErrors reset the error counter after the site has recovered
func (p *Peer) resetErrors() {
	now := time.Now().Unix()
	p.PeerLock.Lock()
	p.Status["LastError"] = ""
	p.Status["LastOnline"] = now
	p.ErrorCount = 0
	p.ErrorLogged = false
	p.Status["PeerStatus"] = PeerStatusUp
	p.PeerLock.Unlock()
}

// UpdateAllTables runs a full update on all dynamic values for all tables which have dynamic updated columns.
// It returns true if the update was successful or false otherwise.
func (p *Peer) UpdateAllTables() bool {
	t1 := time.Now()
	var err error
	restartRequired := false
	for _, n := range Objects.Order {
		t := Objects.Tables[n]
		restartRequired, err = p.UpdateObjectByType(t)
		if err != nil {
			return false
		}
		if restartRequired {
			break
		}
	}
	if err != nil {
		return false
	}
	if restartRequired {
		return p.InitAllTables()
	}
	duration := time.Since(t1)
	if p.StatusGet("PeerStatus").(PeerStatus) != PeerStatusUp {
		log.Infof("[%s] site soft recovered from short outage", p.Name)
	}
	p.resetErrors()
	p.PeerLock.Lock()
	p.Status["ReponseTime"] = duration.Seconds()
	p.Status["LastUpdate"] = time.Now().Unix()
	p.Status["LastFullUpdate"] = time.Now().Unix()
	p.PeerLock.Unlock()
	log.Infof("[%s] update complete in: %s", p.Name, duration.String())
	promPeerUpdates.WithLabelValues(p.Name).Inc()
	promPeerUpdateDuration.WithLabelValues(p.Name).Set(duration.Seconds())
	return true
}

// UpdateDeltaTables runs a delta update on all status, hosts, services, comments and downtimes table.
// It returns true if the update was successful or false otherwise.
func (p *Peer) UpdateDeltaTables() bool {
	t1 := time.Now()

	restartRequired, err := p.UpdateObjectByType(Objects.Tables["status"])
	if restartRequired {
		return p.InitAllTables()
	}
	if err == nil {
		err = p.UpdateDeltaTableHosts("")
	}
	if err == nil {
		err = p.UpdateDeltaTableServices("")
	}
	if err == nil {
		err = p.UpdateDeltaCommentsOrDowntimes("comments")
	}
	if err == nil {
		err = p.UpdateDeltaCommentsOrDowntimes("downtimes")
	}

	duration := time.Since(t1)
	if err != nil {
		if !p.ErrorLogged {
			log.Infof("[%s] updating objects failed after: %s: %s", p.Name, duration.String(), err.Error())
			p.ErrorLogged = true
		} else {
			log.Debugf("[%s] updating objects failed after: %s: %s", p.Name, duration.String(), err.Error())
		}
		return false
	}
	log.Debugf("[%s] delta update complete in: %s", p.Name, duration.String())
	if p.StatusGet("PeerStatus").(PeerStatus) != PeerStatusUp {
		log.Infof("[%s] site soft recovered from short outage", p.Name)
	}
	p.resetErrors()
	p.PeerLock.Lock()
	p.Status["LastUpdate"] = time.Now().Unix()
	p.Status["ReponseTime"] = duration.Seconds()
	p.PeerLock.Unlock()
	promPeerUpdates.WithLabelValues(p.Name).Inc()
	promPeerUpdateDuration.WithLabelValues(p.Name).Set(duration.Seconds())
	return true
}

// UpdateDeltaTableHosts update services by fetching all dynamic data with a last_check filter on the timestamp since
// the previous update with additional UpdateAdditionalDelta seconds.
// It returns any error encountered.
func (p *Peer) UpdateDeltaTableHosts(filterStr string) (err error) {
	// update changed hosts
	table := Objects.Tables["hosts"]
	keys, indexes := table.GetDynamicColumns(p.Flags)
	keys = append(keys, "name")
	if filterStr == "" {
		filterStr = fmt.Sprintf("Filter: last_check >= %v\nFilter: is_executing = 1\nOr: 2\n", (p.StatusGet("LastUpdate").(int64) - UpdateAdditionalDelta))
		// no filter means regular delta update, so lets check if all last_check dates match
		ok, uErr := p.UpdateDeltaTableFullScan(table, filterStr)
		if ok || uErr != nil {
			return uErr
		}
	}
	req := &Request{
		Table:           table.Name,
		Columns:         keys,
		ResponseFixed16: true,
		OutputFormat:    "json",
		FilterStr:       filterStr,
	}
	res, err := p.Query(req)
	if err != nil {
		return
	}
	p.DataLock.Lock()
	nameindex := p.Tables[table.Name].Index
	lastUpdate := p.Tables[table.Name].LastUpdate
	fieldIndex := len(keys) - 1
	now := time.Now().Unix()
	for i := range res {
		resRow := &res[i]
		dataRow := nameindex[(*resRow)[fieldIndex].(string)]
		if dataRow == nil {
			continue
		}
		for j, k := range indexes {
			dataRow[k] = (*resRow)[j]
		}
		lastUpdate[i] = now
	}
	p.DataLock.Unlock()
	promPeerUpdatedHosts.WithLabelValues(p.Name).Add(float64(len(res)))
	log.Debugf("[%s] updated %d hosts", p.Name, len(res))

	return
}

// UpdateDeltaTableServices update services by fetching all dynamic data with a last_check filter on the timestamp since
// the previous update with additional UpdateAdditionalDelta seconds.
// It returns any error encountered.
func (p *Peer) UpdateDeltaTableServices(filterStr string) (err error) {
	// update changed services
	table := Objects.Tables["services"]
	keys, indexes := table.GetDynamicColumns(p.Flags)
	keys = append(keys, []string{"host_name", "description"}...)
	if filterStr == "" {
		filterStr = fmt.Sprintf("Filter: last_check >= %v\nFilter: is_executing = 1\nOr: 2\n", (p.StatusGet("LastUpdate").(int64) - UpdateAdditionalDelta))
		// no filter means regular delta update, so lets check if all last_check dates match
		ok, uErr := p.UpdateDeltaTableFullScan(table, filterStr)
		if ok || uErr != nil {
			return uErr
		}
	}
	req := &Request{
		Table:           table.Name,
		Columns:         keys,
		ResponseFixed16: true,
		OutputFormat:    "json",
		FilterStr:       filterStr,
	}
	res, err := p.Query(req)
	if err != nil {
		return
	}
	p.DataLock.Lock()
	nameindex := p.Tables[table.Name].Index
	fieldIndex1 := len(keys) - 2
	fieldIndex2 := len(keys) - 1
	now := time.Now().Unix()
	for i := range res {
		resRow := &res[i]
		dataRow := nameindex[(*resRow)[fieldIndex1].(string)+";"+(*resRow)[fieldIndex2].(string)]
		if dataRow == nil {
			continue
		}
		for j, k := range indexes {
			dataRow[k] = (*resRow)[j]
		}
		dataRow[0] = now
	}
	p.DataLock.Unlock()
	promPeerUpdatedServices.WithLabelValues(p.Name).Add(float64(len(res)))
	log.Debugf("[%s] updated %d services", p.Name, len(res))

	return
}

// UpdateDeltaTableFullScan updates hosts and services tables by fetching some key indicator fields like last_check
// downtimes or acknowledged status. If an update is required, the last_check timestamp is used as filter for a
// delta update.
// The full scan just returns false without any update if the last update was less then MinFullScanInterval seconds
// ago.
// It returns true if an update was done and any error encountered.
func (p *Peer) UpdateDeltaTableFullScan(table *Table, filterStr string) (updated bool, err error) {
	updated = false
	p.PeerLock.RLock()
	var lastUpdate int64
	if table.Name == "services" {
		lastUpdate = p.Status["LastFullServiceUpdate"].(int64)
	} else if table.Name == "hosts" {
		lastUpdate = p.Status["LastFullHostUpdate"].(int64)
	} else {
		log.Panicf("not implemented for: " + table.Name)
	}
	p.PeerLock.RUnlock()

	// do not do a full scan more often than every 30 seconds
	if lastUpdate > time.Now().Unix()-MinFullScanInterval {
		return
	}

	scanColumns := []string{"last_check",
		"scheduled_downtime_depth",
		"acknowledged",
		"active_checks_enabled",
		"notifications_enabled",
	}
	req := &Request{
		Table:           table.Name,
		Columns:         scanColumns,
		ResponseFixed16: true,
		OutputFormat:    "json",
	}
	res, err := p.Query(req)
	if err != nil {
		return
	}

	indexList := make([]int, len(scanColumns))
	for i, name := range scanColumns {
		indexList[i] = table.GetColumn(name).Index
	}

	missing, err := p.getMissingTimestamps(table, req, &res, indexList)
	if err != nil {
		return
	}

	if len(missing) > 0 {
		filter := []string{filterStr}
		for lastCheck := range missing {
			filter = append(filter, fmt.Sprintf("Filter: last_check = %d\n", int(lastCheck)))
		}
		filter = append(filter, fmt.Sprintf("Or: %d\n", len(filter)))
		if table.Name == "services" {
			err = p.UpdateDeltaTableServices(strings.Join(filter, ""))
		} else if table.Name == "hosts" {
			err = p.UpdateDeltaTableHosts(strings.Join(filter, ""))
		}
	}

	if table.Name == "services" {
		p.StatusSet("LastFullServiceUpdate", time.Now().Unix())
	} else if table.Name == "hosts" {
		p.StatusSet("LastFullHostUpdate", time.Now().Unix())
	}
	updated = true
	return
}

// getMissingTimestamps returns list of last_check dates which can be used to delta update
func (p *Peer) getMissingTimestamps(table *Table, req *Request, res *[][]interface{}, indexList []int) (missing map[float64]bool, err error) {
	missing = make(map[float64]bool)
	p.DataLock.RLock()
	data := p.Tables[table.Name].Data
	if len(data) < len(*res) {
		p.DataLock.RUnlock()
		if p.Flags&Icinga2 == Icinga2 {
			p.checkIcinga2Reload()
			return
		}
		err = &PeerError{msg: fmt.Sprintf("%s cache not ready, got %d entries but only have %d in cache", table.Name, len(*res), len(data)), kind: ResponseError}
		log.Warnf("[%s] %s", p.Name, err.Error())
		p.setBroken(fmt.Sprintf("got more %s than expected. Hint: check clients 'max_response_size' setting.", table.Name))
		return
	}
	for i := range *res {
		row := &(*res)[i]
		for j, index := range indexList {
			if (*row)[j].(float64) != data[i][index].(float64) {
				missing[(*row)[0].(float64)] = true
				break
			}
		}
	}
	p.DataLock.RUnlock()
	return
}

// UpdateDeltaCommentsOrDowntimes update the comments or downtimes table. It fetches the number and highest id of
// the remote comments/downtimes. If an update is required, it then fetches all ids to check which are new and
// which have to be removed.
// It returns any error encountered.
func (p *Peer) UpdateDeltaCommentsOrDowntimes(name string) (err error) {
	// add new comments / downtimes
	table := Objects.Tables[name]

	// get number of entrys and max id
	req := &Request{
		Table:           table.Name,
		ResponseFixed16: true,
		OutputFormat:    "json",
		FilterStr:       "Stats: id != -1\nStats: max id\n",
	}
	res, err := p.Query(req)
	if err != nil {
		return
	}
	var last []interface{}
	p.DataLock.RLock()
	entries := len(p.Tables[table.Name].Data)
	if entries > 0 {
		last = p.Tables[table.Name].Data[entries-1]
	}
	p.DataLock.RUnlock()
	fieldIndex := table.ColumnsIndex["id"]

	if len(res) == 0 || float64(entries) == res[0][0].(float64) && (entries == 0 || last[fieldIndex].(float64) == res[0][1].(float64)) {
		log.Debugf("[%s] %s did not change", p.Name, name)
		return
	}

	// fetch all ids to see which ones are missing or to be removed
	req = &Request{
		Table:           table.Name,
		Columns:         []string{"id"},
		ResponseFixed16: true,
		OutputFormat:    "json",
	}
	res, err = p.Query(req)
	if err != nil {
		return
	}
	p.DataLock.Lock()
	idIndex := p.Tables[table.Name].Index
	missingIds := []string{}
	resIndex := make(map[string]bool)
	for i := range res {
		resRow := &res[i]
		id := fmt.Sprintf("%v", (*resRow)[0])
		_, ok := idIndex[id]
		if !ok {
			log.Debugf("adding %s with id %s", name, id)
			missingIds = append(missingIds, id)
		}
		resIndex[id] = true
	}

	// remove old comments / downtimes
	data := p.Tables[table.Name]
	for id := range idIndex {
		_, ok := resIndex[id]
		if !ok {
			log.Debugf("removing %s with id %s", name, id)
			tmp := idIndex[id]
			data.RemoveItem(tmp)
			delete(idIndex, id)
		}
	}
	p.Tables[table.Name] = data
	p.DataLock.Unlock()

	if len(missingIds) > 0 {
		keys := table.GetInitialKeys(p.Flags)
		req := &Request{
			Table:           table.Name,
			Columns:         keys,
			ResponseFixed16: true,
			OutputFormat:    "json",
			FilterStr:       fmt.Sprintf("Filter: id = %s\nOr: %d\n", strings.Join(missingIds, "\nFilter: id = "), len(missingIds)),
		}
		res, err = p.Query(req)
		if err != nil {
			return
		}
		p.DataLock.Lock()
		data := p.Tables[table.Name]
		for i := range res {
			resRow := res[i]
			id := fmt.Sprintf("%v", resRow[fieldIndex])
			idIndex[id] = resRow
			data.AddItem(&resRow)
		}
		p.Tables[table.Name] = data
		p.DataLock.Unlock()
	}

	log.Debugf("[%s] updated %s", p.Name, name)
	return
}

// query sends the request to a remote livestatus.
// It returns the unmarshaled result and any error encountered.
func (p *Peer) query(req *Request) ([][]interface{}, error) {
	conn, connType, err := p.GetConnection()
	if err != nil {
		log.Debugf("[%s] connection failed: %s", p.Name, err)
		return nil, err
	}
	if conn != nil {
		defer conn.Close()
	}

	query := req.String()
	if log.IsV(3) {
		log.Tracef("[%s] query: %s", p.Name, query)
	}

	p.PeerLock.Lock()
	p.lastRequest = req
	p.lastResponse = nil
	p.Status["Querys"] = p.Status["Querys"].(int) + 1
	totalBytesSend := p.Status["BytesSend"].(int) + len(query)
	p.Status["BytesSend"] = totalBytesSend
	peerAddr := p.Status["PeerAddr"].(string)
	p.PeerLock.Unlock()
	promPeerBytesSend.WithLabelValues(p.Name).Set(float64(totalBytesSend))

	resBytes, err := p.sendTo(req, query, peerAddr, conn, connType)
	if err != nil {
		log.Debugf("[%s] sending data/query failed: %s", p.Name, err)
		return nil, err
	}
	if req.Command != "" {
		return nil, nil
	}

	if log.IsV(3) {
		log.Tracef("[%s] result: %s", p.Name, string(*resBytes))
	}
	p.PeerLock.Lock()
	p.lastResponse = resBytes
	p.PeerLock.Unlock()

	if req.ResponseFixed16 {
		err = p.CheckResponseHeader(resBytes)
		if err != nil {
			return nil, &PeerError{msg: err.Error(), kind: ResponseError, req: req, resBytes: resBytes}
		}
		*resBytes = (*resBytes)[16:]
	}
	return p.parseResult(req, resBytes)
}

func (p *Peer) parseResult(req *Request, resBytes *[]byte) (result [][]interface{}, err error) {
	p.PeerLock.Lock()
	totalBytesReceived := p.Status["BytesReceived"].(int) + len(*resBytes)
	p.Status["BytesReceived"] = totalBytesReceived
	p.PeerLock.Unlock()
	log.Debugf("[%s] got %s answer: size: %d kB", p.Name, req.Table, len(*resBytes)/1024)
	promPeerBytesReceived.WithLabelValues(p.Name).Set(float64(totalBytesReceived))

	if len(*resBytes) == 0 || (string((*resBytes)[0]) != "{" && string((*resBytes)[0]) != "[") {
		err = errors.New(strings.TrimSpace(string(*resBytes)))
		return nil, &PeerError{msg: err.Error(), kind: ResponseError, req: req, resBytes: resBytes}
	}
	if req.OutputFormat == "wrapped_json" {
		dataBytes, _, _, jErr := jsonparser.Get(*resBytes, "data")
		if jErr != nil {
			return nil, &PeerError{msg: jErr.Error(), kind: ResponseError, req: req, resBytes: resBytes}
		}
		resBytes = &dataBytes
	}
	result = make([][]interface{}, 0)
	jsonparser.ArrayEach(*resBytes, func(rowBytes []byte, dataType jsonparser.ValueType, offset int, err error) {
		row, err := djson.Decode(rowBytes)
		result = append(result, row.([]interface{}))
	})

	if err != nil {
		log.Errorf("[%s] json string: %s", p.Name, string(*resBytes))
		log.Errorf("[%s] json error: %s", p.Name, err.Error())
		return nil, &PeerError{msg: err.Error(), kind: ResponseError}
	}

	return
}

func (p *Peer) sendTo(req *Request, query string, peerAddr string, conn net.Conn, connType string) (*[]byte, error) {
	// http connections
	if connType == "http" {
		res, err := p.HTTPQueryWithRetrys(peerAddr, query, 2)
		if err != nil {
			return nil, err
		}
		// commands do not send anything back
		if req.Command != "" {
			return nil, nil
		}
		return &res, nil
	}

	// tcp/unix connections
	// set read timeout
	conn.SetDeadline(time.Now().Add(time.Duration(p.LocalConfig.NetTimeout) * time.Second))
	fmt.Fprintf(conn, "%s", query)

	// close write part of connection
	switch c := conn.(type) {
	case *net.TCPConn:
		c.CloseWrite()
	case *net.UnixConn:
		c.CloseWrite()
	}

	// read result from connection into result buffer
	buf := new(bytes.Buffer)
	for {
		_, err := io.CopyN(buf, conn, 65536)
		if err != nil {
			if err != io.EOF {
				return nil, err
			}
			break
		}
	}

	// commands do not send anything back, return after reading any response to be sure the server accepted the command
	if req.Command != "" {
		return nil, nil
	}

	res := buf.Bytes()
	return &res, nil
}

// Query sends a livestatus request from a request object.
// It calls query and logs all errors except connection errors which are logged in GetConnection.
// It returns the livestatus result and any error encountered.
func (p *Peer) Query(req *Request) (result [][]interface{}, err error) {
	result, err = p.query(req)
	if err != nil {
		p.setNextAddrFromErr(err)
	}
	return
}

// QueryString sends a livestatus request from a given string.
// It returns the livestatus result and any error encountered.
func (p *Peer) QueryString(str string) ([][]interface{}, error) {
	req, _, err := NewRequest(bufio.NewReader(bytes.NewBufferString(str)))
	if err != nil {
		return nil, err
	}
	if req == nil {
		err = errors.New("bad request: empty request")
		return nil, err
	}
	return p.Query(req)
}

// CheckResponseHeader verifies the return code and content length of livestatus answer.
// It returns an error if something is wrong with the header.
func (p *Peer) CheckResponseHeader(resBytes *[]byte) (err error) {
	resSize := len(*resBytes)
	if resSize < 16 {
		err = fmt.Errorf("[%s] uncomplete response header: %s", p.Name, string(*resBytes))
		return
	}
	header := (*resBytes)[0:15]
	resSize = resSize - 16

	matched := reResponseHeader.FindStringSubmatch(string(header))
	if len(matched) != 3 {
		err = fmt.Errorf("[%s] uncomplete response header: %s", p.Name, string(header))
		return
	}
	resCode, _ := strconv.Atoi(matched[1])
	expSize, _ := strconv.Atoi(matched[2])

	if resCode != 200 {
		err = fmt.Errorf("[%s] bad response: %s", p.Name, string(*resBytes))
		return
	}
	if expSize != resSize {
		err = fmt.Errorf("[%s] bad response size, expected %d, got %d", p.Name, expSize, resSize)
		return
	}
	return
}

// GetConnection returns the next net.Conn object which answers to a connect.
// In case of a http connection, it just trys a tcp connect, but does not
// return anything.
// It returns the connection object and any error encountered.
func (p *Peer) GetConnection() (conn net.Conn, connType string, err error) {
	numSources := len(p.Source)

	for x := 0; x < numSources; x++ {
		peerAddr := p.StatusGet("PeerAddr").(string)
		connType = "unix"
		if strings.HasPrefix(peerAddr, "http") {
			connType = "http"
		} else if strings.Contains(peerAddr, ":") {
			connType = "tcp"
		}
		switch connType {
		case "tcp":
			fallthrough
		case "unix":
			conn, err = net.DialTimeout(connType, peerAddr, time.Duration(p.LocalConfig.ConnectTimeout)*time.Second)
		case "http":
			// test at least basic tcp connect
			uri, uErr := url.Parse(peerAddr)
			if uErr != nil {
				err = uErr
			}
			host := uri.Host
			if !strings.Contains(host, ":") {
				switch uri.Scheme {
				case "http":
					host = host + ":80"
				case "https":
					host = host + ":443"
				default:
					err = &PeerError{msg: fmt.Sprintf("unknown scheme: %s", uri.Scheme), kind: ConnectionError}
				}
			}
			conn, err = net.DialTimeout("tcp", host, time.Duration(p.LocalConfig.ConnectTimeout)*time.Second)
			if conn != nil {
				conn.Close()
			}
			conn = nil
		}
		// connection successful
		if err == nil {
			promPeerConnections.WithLabelValues(p.Name).Inc()
			if x > 0 {
				log.Infof("[%s] active source changed to %s", p.Name, peerAddr)
				p.Flags = NoFlags
			}
			return
		}

		// connection error
		p.setNextAddrFromErr(err)
	}

	return nil, "", &PeerError{msg: err.Error(), kind: ConnectionError}
}

func (p *Peer) setNextAddrFromErr(err error) {
	promPeerFailedConnections.WithLabelValues(p.Name).Inc()
	p.PeerLock.Lock()
	peerAddr := p.Status["PeerAddr"].(string)
	log.Debugf("[%s] connection error %s: %s", p.Name, peerAddr, err)
	defer p.PeerLock.Unlock()
	p.Status["LastError"] = err.Error()
	p.ErrorCount++

	numSources := len(p.Source)

	// try next node if there are multiple
	curNum := p.Status["CurPeerAddrNum"].(int)
	nextNum := curNum
	nextNum++
	if nextNum >= numSources {
		nextNum = 0
	}
	p.Status["CurPeerAddrNum"] = nextNum
	p.Status["PeerAddr"] = p.Source[nextNum]

	if p.Status["PeerStatus"].(PeerStatus) == PeerStatusUp || p.Status["PeerStatus"].(PeerStatus) == PeerStatusPending {
		p.Status["PeerStatus"] = PeerStatusWarning
	}
	now := time.Now().Unix()
	lastOnline := p.Status["LastOnline"].(int64)
	log.Debugf("[%s] last online: %s", p.Name, timeOrNever(lastOnline))
	if lastOnline < now-int64(p.LocalConfig.StaleBackendTimeout) || (p.ErrorCount > numSources && lastOnline <= 0) {
		if p.Status["PeerStatus"].(PeerStatus) != PeerStatusDown {
			log.Warnf("[%s] site went offline: %s", p.Name, err.Error())
			// clear existing data from memory
			p.DataLock.Lock()
			p.Clear()
			p.DataLock.Unlock()
		}
		p.Status["PeerStatus"] = PeerStatusDown
	}

	if numSources > 1 {
		log.Debugf("[%s] trying next one: %s", p.Name, peerAddr)
	}
}

// CreateObjectByType fetches all static and dynamic data from the remote site and creates the initial table.
// It returns any error encountered.
func (p *Peer) CreateObjectByType(table *Table) (_, err error) {
	// log table does not create objects
	if table.PassthroughOnly {
		return
	}

	keys := table.GetInitialKeys(p.Flags)
	refs := make(map[string][][]interface{})
	index := make(map[string][]interface{})

	// complete virtual table ends here
	if len(keys) == 0 || table.Virtual {
		p.DataLock.Lock()
		p.Tables[table.Name] = DataTable{Table: table, Data: make([][]interface{}, 1), Refs: refs, Index: index}
		p.DataLock.Unlock()
		return
	}

	var res [][]interface{}
	if table.GroupBy {
		// calculate groupby data from local table
		res, err = p.GetGroupByData(table)
	} else {
		// fetch remote objects
		req := &Request{
			Table:           table.Name,
			Columns:         keys,
			ResponseFixed16: true,
			OutputFormat:    "json",
		}
		res, err = p.Query(req)
	}
	if err != nil {
		return
	}
	if !table.GroupBy {
		log.Debugf("[%s] fetched %d initial %s objects", p.Name, len(res), table.Name)
	}

	// expand references, create a hash entry for each reference type, ex.: hosts
	// with an array containing the references (using the same index as the original row)
	for _, refNum := range table.RefColCacheIndexes {
		refCol := table.Columns[refNum]
		fieldName := refCol.Name
		refs[fieldName] = make([][]interface{}, len(res))
		p.DataLock.RLock()
		refByName := p.Tables[fieldName].Index
		if refCol.Name == "services" {
			hostnameIndex := table.ColumnsIndex["hostname_index"]
			for i := range res {
				row := res[i]
				key := row[hostnameIndex].(string) + ";" + row[refCol.RefIndex].(string)
				refs[fieldName][i] = refByName[key]
				if refByName[key] == nil {
					log.Panicf("%s '%s' ref not found from table %s, refmap contains %d elements", refCol.Name, row[refCol.RefIndex].(string), table.Name, len(refByName))
				}
			}
		} else {
			for i := range res {
				row := res[i]
				refs[fieldName][i] = refByName[row[refCol.RefIndex].(string)]
				if refByName[row[refCol.RefIndex].(string)] == nil {
					log.Panicf("%s '%s' ref not found from table %s, refmap contains %d elements", refCol.Name, row[refCol.RefIndex].(string), table.Name, len(refByName))
				}
			}
		}
		p.DataLock.RUnlock()
	}

	p.createIndex(table, &res, &index)
	p.createFlags(table, &res, &index)

	now := time.Now().Unix()

	lastUpdate := make([]int64, 0)
	if _, ok := table.ColumnsIndex["lmd_last_cache_update"]; ok {
		lastUpdate = make([]int64, len(res))
		for i := range res {
			lastUpdate[i] = now
		}
	}

	p.DataLock.Lock()
	p.Tables[table.Name] = DataTable{Table: table, Data: res, Refs: refs, Index: index, LastUpdate: lastUpdate}
	p.DataLock.Unlock()
	p.PeerLock.Lock()
	p.Status["LastUpdate"] = now
	p.Status["LastFullUpdate"] = now
	p.PeerLock.Unlock()
	return
}

func (p *Peer) createIndex(table *Table, res *[][]interface{}, index *map[string][]interface{}) {
	// create host lookup indexes
	if table.Name == "hosts" {
		indexField := table.ColumnsIndex["name"]
		for i := range *res {
			row := (*res)[i]
			(*index)[row[indexField].(string)] = row
		}
		promHostCount.WithLabelValues(p.Name).Set(float64(len((*res))))
	}
	// create service lookup indexes
	if table.Name == "services" {
		indexField1 := table.ColumnsIndex["host_name"]
		indexField2 := table.ColumnsIndex["description"]
		for i := range *res {
			row := (*res)[i]
			(*index)[row[indexField1].(string)+";"+row[indexField2].(string)] = row
		}
		promServiceCount.WithLabelValues(p.Name).Set(float64(len((*res))))
	}
	if table.Name == "hostgroups" || table.Name == "servicegroups" {
		indexField := table.ColumnsIndex["name"]
		for i := range *res {
			row := (*res)[i]
			(*index)[row[indexField].(string)] = row
		}
	}
	// create downtime / comment id lookup indexes
	if table.Name == "comments" || table.Name == "downtimes" {
		indexField := table.ColumnsIndex["id"]
		for i := range *res {
			row := (*res)[i]
			(*index)[fmt.Sprintf("%v", row[indexField])] = row
		}
	}
}

func (p *Peer) createFlags(table *Table, res *[][]interface{}, index *map[string][]interface{}) {
	// set backend specific flags
	if table.Name == "status" && len(*res) > 0 {
		p.PeerLock.Lock()
		defer p.PeerLock.Unlock()
		row := (*res)[0]
		if len(reShinkenVersion.FindStringSubmatch(row[table.GetColumn("livestatus_version").Index].(string))) > 0 {
			log.Debugf("[%s] remote connection Shinken flag set", p.Name)
			p.Flags |= Shinken
		}
		if len(reIcinga2Version.FindStringSubmatch(row[table.GetColumn("livestatus_version").Index].(string))) > 0 {
			log.Debugf("[%s] remote connection Icinga2 flag set", p.Name)
			p.Flags |= Icinga2
		}
		// getting more than one status is a sure sign for a LMD backend
		if len(*res) > 1 {
			log.Debugf("[%s] remote connection LMD flag set", p.Name)
			p.Flags |= LMD
		}
	}
}

// GetGroupByData returns fake query result for given groupby table
func (p *Peer) GetGroupByData(table *Table) (res [][]interface{}, err error) {
	res = make([][]interface{}, 0)
	p.DataLock.RLock()
	defer p.DataLock.RUnlock()
	switch table.Name {
	case "hostsbygroup":
		index1 := p.Tables["hosts"].Table.ColumnsIndex["name"]
		index2 := p.Tables["hosts"].Table.ColumnsIndex["groups"]
		for _, row := range p.Tables["hosts"].Data {
			name := row[index1].(string)
			switch v := (row[index2]).(type) {
			case []interface{}:
				for _, group := range v {
					res = append(res, []interface{}{name, group})
				}
			}
		}
	case "servicesbygroup":
		index1 := p.Tables["services"].Table.ColumnsIndex["host_name"]
		index2 := p.Tables["services"].Table.ColumnsIndex["description"]
		index3 := p.Tables["services"].Table.ColumnsIndex["groups"]
		for _, row := range p.Tables["services"].Data {
			hostName := row[index1].(string)
			description := row[index2].(string)
			switch v := (row[index3]).(type) {
			case []interface{}:
				for _, group := range v {
					res = append(res, []interface{}{hostName, description, group})
				}
			}
		}
	case "servicesbyhostgroup":
		index1 := p.Tables["services"].Table.ColumnsIndex["host_name"]
		index2 := p.Tables["services"].Table.ColumnsIndex["description"]
		hostGroupsColumn := p.Tables["services"].Table.GetResultColumn("host_groups")
		refs := p.Tables["services"].Refs
		for rowNum, row := range p.Tables["services"].Data {
			hostName := row[index1].(string)
			description := row[index2].(string)
			groups := p.GetRowValue(hostGroupsColumn, &row, rowNum, p.Tables["services"].Table, &refs)
			switch v := (groups).(type) {
			case []interface{}:
				for _, group := range v {
					res = append(res, []interface{}{hostName, description, group})
				}
			}
		}
	default:
		log.Panicf("GetGroupByData not implemented for table: %s", table.Name)
	}
	return
}

// UpdateObjectByType updates a given table by requesting all dynamic columns from the remote peer.
// Assuming we get the objects always in the same order, we can just iterate over the index and update the fields.
// It returns a boolean flag whether the remote site has been restarted and any error encountered.
func (p *Peer) UpdateObjectByType(table *Table) (restartRequired bool, err error) {
	if len(table.DynamicColCacheNames) == 0 {
		return
	}
	if table.Virtual {
		return
	}
	// no updates for passthrough tables, ex.: log
	if table.PassthroughOnly {
		return
	}
	keys, indexes := table.GetDynamicColumns(p.Flags)
	req := &Request{
		Table:           table.Name,
		Columns:         keys,
		ResponseFixed16: true,
		OutputFormat:    "json",
	}
	res, err := p.Query(req)
	if err != nil {
		return
	}
	p.DataLock.RLock()
	data := p.Tables[table.Name].Data
	p.DataLock.RUnlock()
	if len(res) > len(data) {
		log.Debugf("[%s] site returned to many objects, assuming backend has been restarted", p.Name)
		restartRequired = true
		return
	}

	if table.Name == "timeperiods" {
		// check for changed timeperiods, because we have to update the linked hosts and services as well
		p.updateTimeperiodsData(table, res, indexes)
	} else {
		p.DataLock.Lock()
		_, hasLastCacheUpdate := table.ColumnsIndex["lmd_last_cache_update"]
		now := time.Now().Unix()
		lastUpdate := p.Tables[table.Name].LastUpdate
		indexLength := len(indexes)
		for i := range res {
			row := res[i]
			if len(row) < indexLength {
				err = fmt.Errorf("response list has wrong size, got %d and expexted %d", len(row), indexLength)
				p.DataLock.Unlock()
				return
			}
			for j, k := range indexes {
				data[i][k] = row[j]
			}
			if hasLastCacheUpdate {
				lastUpdate[i] = now
			}
		}
		p.DataLock.Unlock()
	}

	switch table.Name {
	case "hosts":
		promPeerUpdatedHosts.WithLabelValues(p.Name).Add(float64(len(res)))
	case "services":
		promPeerUpdatedServices.WithLabelValues(p.Name).Add(float64(len(res)))
	case "status":
		if p.StatusGet("ProgramStart") != data[0][table.ColumnsIndex["program_start"]] {
			log.Infof("[%s] site has been restarted, recreating objects", p.Name)
			restartRequired = true
		}
		return
	}
	return
}

func (p *Peer) updateTimeperiodsData(table *Table, res [][]interface{}, indexes []int) {
	changedTimeperiods := make(map[string]float64)
	nameIndex := table.ColumnsIndex["name"]
	p.DataLock.Lock()
	data := p.Tables[table.Name].Data
	now := time.Now().Unix()
	for i := range res {
		row := res[i]
		for j, k := range indexes {
			if data[i][k] != row[j] {
				changedTimeperiods[data[i][nameIndex].(string)] = data[i][k].(float64)
			}
			data[i][k] = row[j]
			data[i][0] = now
		}
	}
	p.DataLock.Unlock()
	// Update hosts and services with those changed timeperiods
	for name, state := range changedTimeperiods {
		log.Debugf("[%s] timeperiod %s has changed to %v, need to update affected hosts/services", p.Name, name, state)
		p.UpdateDeltaTableHosts("Filter: check_period = " + name + "\nFilter: notification_period = " + name + "\nOr: 2\n")
		p.UpdateDeltaTableServices("Filter: check_period = " + name + "\nFilter: notification_period = " + name + "\nOr: 2\n")
	}
}

// GetRowValue returns the value for a given index in a data row and resolves
// any virtual or reference column.
// The result is returned as interface.
func (p *Peer) GetRowValue(col *ResultColumn, row *[]interface{}, rowNum int, table *Table, refs *map[string][][]interface{}) interface{} {
	if col.Column.Index >= len(*row) {
		if col.Type == VirtCol {
			return p.GetVirtRowValue(col, row, rowNum, table, refs)
		}

		// this happens if we are requesting an optional column from the wrong backend
		// ex.: shinken specific columns from a non-shinken backend
		if col.Column.RefIndex == 0 {
			if _, ok := (*refs)[col.Column.Name]; !ok {
				// return empty placeholder matching the column type
				return (col.Column.GetEmptyValue())
			}
		}

		// reference columns
		refObj := (*refs)[table.Columns[col.Column.RefIndex].Name][rowNum]
		if refObj == nil {
			log.Panicf("should not happen, ref not found in table %s", table.Name)
		}
		if len(refObj) > col.Column.RefColIndex {
			return refObj[col.Column.RefColIndex]
		}

		// this happens if we are requesting an optional column from the wrong backend
		// ex.: shinken specific columns from a non-shinken backend
		// -> return empty placeholder matching the column type
		return (col.Column.GetEmptyValue())
	}
	return (*row)[col.Column.Index]
}

// GetVirtRowValue returns the actual value for a virtual column.
func (p *Peer) GetVirtRowValue(col *ResultColumn, row *[]interface{}, rowNum int, table *Table, refs *map[string][][]interface{}) interface{} {
	p.PeerLock.RLock()
	value, ok := p.Status[col.Column.VirtMap.Key]
	p.PeerLock.RUnlock()
	if !ok {
		value = p.GetVirtRowComputedValue(col, row, rowNum, table, refs)
	}
	colType := col.Column.VirtType
	switch colType {
	case IntCol:
		fallthrough
	case FloatCol:
		return numberToFloat(&value)
	case StringCol:
		return value
	case TimeCol:
		val := value.(int64)
		if val < 0 {
			val = 0
		}
		return val
	default:
		log.Panicf("not implemented")
	}
	return nil
}

// GetVirtRowComputedValue returns a computed virtual value for the given column.
func (p *Peer) GetVirtRowComputedValue(col *ResultColumn, row *[]interface{}, rowNum int, table *Table, refs *map[string][][]interface{}) (value interface{}) {
	switch col.Name {
	case "lmd_last_cache_update":
		// return timestamp of last update for this data row
		value = p.Tables[table.Name].LastUpdate[rowNum]
	case "last_state_change_order":
		// return last_state_change or program_start
		lastStateChange := numberToFloat(&((*row)[table.ColumnsIndex["last_state_change"]]))
		if lastStateChange == 0 {
			value = p.Status["ProgramStart"]
		} else {
			value = lastStateChange
		}
	case "host_last_state_change_order":
		// return last_state_change or program_start
		val := p.GetRowValue(table.GetResultColumn("host_last_state_change"), row, rowNum, table, refs)
		lastStateChange := numberToFloat(&val)
		if lastStateChange == 0 {
			value = p.Status["ProgramStart"]
		} else {
			value = lastStateChange
		}
	case "state_order":
		// return 4 instead of 2, which makes critical come first
		// this way we can use this column to sort by state
		state := numberToFloat(&((*row)[table.ColumnsIndex["state"]]))
		if state == 2 {
			value = 4
		} else {
			value = state
		}
	case "has_long_plugin_output":
		// return 1 if there is long_plugin_output
		val := (*row)[table.ColumnsIndex["long_plugin_output"]].(string)
		if val != "" {
			value = 1
		} else {
			value = 0
		}
	case "host_has_long_plugin_output":
		// return 1 if there is long_plugin_output
		val := p.GetRowValue(table.GetResultColumn("long_plugin_output"), row, rowNum, table, refs).(string)
		if val != "" {
			value = 1
		} else {
			value = 0
		}
	default:
		log.Panicf("cannot handle virtual column: %s", col.Name)
	}
	return
}

// WaitCondition waits for a given condition.
// It returns true if the wait timed out or false if the condition matched successfully.
func (p *Peer) WaitCondition(req *Request) bool {
	c := make(chan struct{})
	go func() {
		// make sure we log panics properly
		defer logPanicExit()

		p.DataLock.RLock()
		table := p.Tables[req.Table].Table
		refs := p.Tables[req.Table].Refs
		p.DataLock.RUnlock()
		var lastUpdate int64
		for {
			select {
			case <-c:
				// canceled
				return
			default:
			}
			// waiting for final update
			if lastUpdate > 0 {
				curUpdate := p.StatusGet("LastUpdate").(int64)
				// wait up to WaitTimeout till the update is complete
				if curUpdate > lastUpdate {
					close(c)
					return
				}
				time.Sleep(time.Millisecond * 200)
				continue
			}
			// get object to watch
			var obj []interface{}
			if req.Table == "hosts" || req.Table == "services" {
				p.DataLock.RLock()
				obj = p.Tables[req.Table].Index[req.WaitObject]
				p.DataLock.RUnlock()
			} else {
				log.Errorf("unsupported wait table: %s", req.Table)
				close(c)
				return
			}
			if p.MatchRowFilter(table, &refs, req.WaitCondition[0], &obj, 0) {
				// trigger update for all, wait conditions are run against the last object
				// but multiple commands may have been sent
				p.ScheduleImmediateUpdate()
				lastUpdate = p.StatusGet("LastUpdate").(int64)
				continue
			}
			time.Sleep(time.Millisecond * 200)
			if req.Table == "hosts" {
				p.UpdateDeltaTableHosts("Filter: name = " + req.WaitObject + "\n")
			} else if req.Table == "services" {
				tmp := strings.SplitN(req.WaitObject, ";", 2)
				if len(tmp) < 2 {
					log.Errorf("unsupported service wait object: %s", req.WaitObject)
					close(c)
					return
				}
				p.UpdateDeltaTableServices("Filter: host_name = " + tmp[0] + "\nFilter: description = " + tmp[1] + "\n")
			}
		}
	}()
	select {
	case <-c:
		return false // completed normally
	case <-time.After(time.Duration(req.WaitTimeout) * time.Millisecond):
		close(c)
		return true // timed out
	}
}

// HTTPQueryWithRetrys calls HTTPQuery with given amount of retries.
func (p *Peer) HTTPQueryWithRetrys(peerAddr string, query string, retries int) (res []byte, err error) {
	res, err = p.HTTPQuery(peerAddr, query)

	// retry on broken pipe errors
	for retry := 1; retry <= retries && err != nil; retry++ {
		if strings.HasPrefix(err.Error(), "remote site returned rc: 0 - ERROR: broken pipe.") {
			time.Sleep(1 * time.Second)
			res, err = p.HTTPQuery(peerAddr, query)
			if err == nil {
				log.Debugf("[%s] site returned successful result after %d retries", p.Name, retry)
			}
		}
	}
	return
}

// HTTPQuery sends a query over http to a Thruk backend.
// It returns the livestatus answers and any encountered error.
func (p *Peer) HTTPQuery(peerAddr string, query string) (res []byte, err error) {
	options := make(map[string]interface{})
	if p.Config.RemoteName != "" {
		options["backends"] = []string{p.Config.RemoteName}
	}
	options["action"] = "raw"
	options["sub"] = "_raw_query"
	options["args"] = []string{strings.TrimSpace(query) + "\n"}
	optionStr, _ := json.Marshal(options)
	netClient.Timeout = time.Duration(p.LocalConfig.NetTimeout) * time.Second
	response, err := netClient.PostForm(completePeerHTTPAddr(peerAddr), url.Values{
		"data": {fmt.Sprintf("{\"credential\": \"%s\", \"options\": %s}", p.Config.Auth, optionStr)},
	})
	if err != nil {
		return
	}
	contents, err := ExtractHTTPResponse(response)
	if err != nil {
		return
	}

	if len(contents) < 1 || contents[0] != '{' {
		if len(contents) > 50 {
			contents = contents[:50]
		}
		err = &PeerError{msg: fmt.Sprintf("site did not return a proper response: %s", contents), kind: ResponseError}
		return
	}
	var result HTTPResult
	err = json.Unmarshal(contents, &result)
	if err != nil {
		return
	}
	if result.Rc != 0 {
		err = &PeerError{msg: fmt.Sprintf("remote site returned rc: %d - %s", result.Rc, result.Output), kind: ResponseError}
		return
	}

	var output []interface{}
	err = json.Unmarshal(result.Output, &output)
	if err != nil {
		return
	}
	remoteError := ""
	if log.IsV(3) {
		log.Tracef("[%s] response: %s", p.Name, result.Output)
	}
	if len(output) >= 4 {
		if v, ok := output[3].(string); ok {
			remoteError = strings.TrimSpace(v)
			matched := reHTTPTooOld.FindStringSubmatch(remoteError)
			if len(matched) > 0 {
				err = &PeerError{msg: fmt.Sprintf("remote site too old: v%s - %s", result.Version, result.Branch), kind: ResponseError}
			} else {
				err = &PeerError{msg: fmt.Sprintf("remote site returned rc: %d - %s", result.Rc, remoteError), kind: ResponseError}
			}
			return
		}
	}
	if v, ok := output[2].(string); ok {
		res = []byte(v)
	} else {
		err = &PeerError{msg: fmt.Sprintf("unknown site error, got: %v", result), kind: ResponseError}
	}
	return
}

// ExtractHTTPResponse returns the content of a HTTP request.
func ExtractHTTPResponse(response *http.Response) (contents []byte, err error) {
	contents, hErr := ioutil.ReadAll(response.Body)
	io.Copy(ioutil.Discard, response.Body)
	response.Body.Close()
	if hErr != nil {
		err = hErr
		return
	}
	if response.StatusCode != 200 {
		matched := reHTTPOMDError.FindStringSubmatch(string(contents))
		if len(matched) > 1 {
			err = &PeerError{msg: fmt.Sprintf("http request failed: %s - %s", response.Status, matched[1]), kind: ResponseError}
		} else {
			err = &PeerError{msg: fmt.Sprintf("http request failed: %s", response.Status), kind: ResponseError}
		}
	}
	return
}

// SpinUpPeers starts an immediate parallel delta update for all supplied peer ids.
func SpinUpPeers(peers []string) {
	waitgroup := &sync.WaitGroup{}
	for _, id := range peers {
		p := DataStore[id]
		waitgroup.Add(1)
		go func(peer *Peer, wg *sync.WaitGroup) {
			// make sure we log panics properly
			defer logPanicExit()

			defer wg.Done()
			p.StatusSet("Idling", false)
			log.Infof("[%s] switched back to normal update interval", peer.Name)
			if peer.StatusGet("PeerStatus").(PeerStatus) == PeerStatusUp {
				log.Debugf("[%s] spin up update", peer.Name)
				peer.UpdateObjectByType(Objects.Tables["timeperiods"])
				peer.UpdateDeltaTables()
				log.Debugf("[%s] spin up update done", peer.Name)
			} else {
				// force new update sooner
				peer.StatusSet("LastUpdate", time.Now().Unix()-peer.LocalConfig.Updateinterval)
			}
		}(p, waitgroup)
	}
	waitTimeout(waitgroup, 5)
	log.Debugf("spin up completed")
}

// BuildLocalResponseData returnss the result data for a given request
func (p *Peer) BuildLocalResponseData(res *Response, indexes *[]int) (int, *[][]interface{}, *map[string][]*Filter) {
	req := res.Request
	numPerRow := len(*indexes)
	log.Tracef("BuildLocalResponseData: %s", p.Name)
	p.DataLock.RLock()
	table := p.Tables[req.Table].Table
	p.DataLock.RUnlock()
	if table == nil || (!p.isOnline() && !table.Virtual) {
		res.Lock.Lock()
		res.Failed[p.ID] = fmt.Sprintf("%v", p.StatusGet("LastError"))
		res.Lock.Unlock()
		return 0, nil, nil
	}

	// if a WaitTrigger is supplied, wait max ms till the condition is true
	if req.WaitTrigger != "" {
		p.WaitCondition(req)
	}

	p.DataLock.RLock()
	defer p.DataLock.RUnlock()
	data := p.Tables[req.Table].Data

	// get data for special tables
	if table.Name == "tables" || table.Name == "columns" {
		data = Objects.GetTableColumnsData()
	}

	if len(data) == 0 {
		return 0, nil, nil
	}

	if len(res.Request.Stats) > 0 {
		return 0, nil, p.gatherStatsResult(res, table, &data, numPerRow, indexes)
	}
	total, result := p.gatherResultRows(res, table, &data, numPerRow, indexes)
	return total, result, nil
}

// isOnline returns true if this peer is online and has data
func (p *Peer) isOnline() bool {
	status := p.StatusGet("PeerStatus").(PeerStatus)
	if status == PeerStatusUp || status == PeerStatusWarning {
		return true
	}
	return false
}

func (p *Peer) gatherResultRows(res *Response, table *Table, data *[][]interface{}, numPerRow int, indexes *[]int) (int, *[][]interface{}) {
	req := res.Request
	refs := p.Tables[req.Table].Refs
	result := make([][]interface{}, 0)

	// if there is no sort header or sort by name only,
	// we can drastically reduce the result set by applying the limit here already
	limit := optimizeResultLimit(req, table)

	found := 0
Rows:
	for j := range *data {
		row := &((*data)[j])
		// does our filter match?
		for _, f := range req.Filter {
			if !p.MatchRowFilter(table, &refs, f, row, j) {
				continue Rows
			}
		}
		found++
		// check if we have enough result rows already
		// we still need to count how many result we would have...
		if limit > 0 && found > limit {
			continue Rows
		}

		// build result row
		resRow := make([]interface{}, numPerRow)
		for k, i := range *(indexes) {
			if i < 0 {
				// virtual columns
				resRow[k] = p.GetRowValue(&(res.Columns[k]), row, j, table, &refs)
			} else {
				// check if this is a reference column
				// reference columns come after the non-ref columns
				if i >= len(*row) {
					resRow[k] = p.GetRowValue(&(res.Columns[k]), row, j, table, &refs)
				} else {
					resRow[k] = (*row)[i]
				}
			}
			// fill null values with something useful
			if resRow[k] == nil {
				resRow[k] = table.Columns[i].GetEmptyValue()
			}
		}
		result = append(result, resRow)
	}

	// sanitize broken custom var data from icinga2
	for j, i := range *(indexes) {
		if i > 0 && table.Columns[i].Type == CustomVarCol {
			for k := range result {
				resRow := &(result[k])
				(*resRow)[j] = interfaceToCustomVarHash(&(*resRow)[j])
			}
		}
	}

	return found, &result
}

func (p *Peer) gatherStatsResult(res *Response, table *Table, data *[][]interface{}, numPerRow int, indexes *[]int) *map[string][]*Filter {
	req := res.Request
	refs := p.Tables[req.Table].Refs

	localStats := make(map[string][]*Filter)

Rows:
	for j := range *data {
		row := &((*data)[j])
		// does our filter match?
		for _, f := range req.Filter {
			if !p.MatchRowFilter(table, &refs, f, row, j) {
				continue Rows
			}
		}

		key := ""
		if len(req.Columns) > 0 {
			key = p.getStatsKey(&res.Columns, table, &refs, row, j)
		}

		if _, ok := localStats[key]; !ok {
			localStats[key] = createLocalStatsCopy(&req.Stats)
		}

		// count stats
		for i, s := range req.Stats {
			// avg/sum/min/max are passed through, they dont have filter
			// counter must match their filter
			if s.StatsType == Counter {
				if p.MatchRowFilter(table, &refs, s, row, j) {
					localStats[key][i].Stats++
					localStats[key][i].StatsCount++
				}
			} else {
				val := p.GetRowValue(s.Column, row, j, table, &refs)
				localStats[key][i].ApplyValue(numberToFloat(&val), 1)
			}
		}
	}

	return &localStats
}

func createLocalStatsCopy(stats *[]*Filter) []*Filter {
	localStats := make([]*Filter, len(*stats))
	for i, s := range *stats {
		localStats[i] = &Filter{}
		localStats[i].StatsType = s.StatsType
		if s.StatsType == Min {
			localStats[i].Stats = -1
		}
	}
	return localStats
}
func optimizeResultLimit(req *Request, table *Table) (limit int) {
	if req.Limit > 0 && table.IsDefaultSortOrder(&req.Sort) {
		limit = req.Limit
		if req.Offset > 0 {
			limit += req.Offset
		}
	}
	return
}

func (p *Peer) getStatsKey(columns *[]ResultColumn, table *Table, refs *map[string][][]interface{}, row *[]interface{}, rowNum int) string {
	keyValues := []string{}
	for _, col := range *columns {
		value := p.GetRowValue(&col, row, rowNum, table, refs)
		keyValues = append(keyValues, fmt.Sprintf("%v", value))
	}
	return strings.Join(keyValues, ";")
}

func (p *Peer) clearLastRequest() {
	p.PeerLock.Lock()
	p.lastRequest = nil
	p.lastResponse = nil
	p.PeerLock.Unlock()
}

// MatchRowFilter returns true if the given filter matches the given datarow.
func (p *Peer) MatchRowFilter(table *Table, refs *map[string][][]interface{}, filter *Filter, row *[]interface{}, rowNum int) bool {
	// recursive group filter
	filterLength := len(filter.Filter)
	if filterLength > 0 {
		for _, f := range filter.Filter {
			subresult := p.MatchRowFilter(table, refs, f, row, rowNum)
			switch filter.GroupOperator {
			case And:
				// if all conditions must match and we failed already, exit early
				if !subresult {
					return false
				}
			case Or:
				// if only one condition must match and we got that already, exit early
				if subresult {
					return true
				}
			}
		}
		// if this is an AND filter and we did not return yet, this means all have matched.
		// else its an OR filter and none has matched so far.
		return filter.GroupOperator == And
	}

	// normal field filter
	if filter.Column.Column.Index < len(*row) {
		// directly access the row value
		return (filter.MatchFilter(&((*row)[filter.Column.Column.Index])))
	}
	value := p.GetRowValue(filter.Column, row, rowNum, table, refs)
	return (filter.MatchFilter(&value))
}

func (p *Peer) checkIcinga2Reload() bool {
	if p.Flags&Icinga2 == Icinga2 && p.hasChanged() {
		return (p.InitAllTables())
	}
	return (true)
}

// completePeerHTTPAddr returns autocompleted address for peer
// it appends /thruk/cgi-bin/remote.cgi or parts of it
func completePeerHTTPAddr(addr string) string {
	switch {
	case regexp.MustCompile(`/thruk/$`).MatchString(addr):
		return addr + "cgi-bin/remote.cgi"
	case regexp.MustCompile(`/thruk$`).MatchString(addr):
		return addr + "/cgi-bin/remote.cgi"
	case regexp.MustCompile(`/remote\.cgi$`).MatchString(addr):
		return addr
	case regexp.MustCompile(`/$`).MatchString(addr):
		return addr + "thruk/cgi-bin/remote.cgi"
	}
	return addr + "/thruk/cgi-bin/remote.cgi"
}

func (p *Peer) setBroken(details string) {
	log.Warnf("[%s] %s", p.Name, details)
	p.PeerLock.Lock()
	p.Status["PeerStatus"] = PeerStatusBroken
	p.Status["LastError"] = "broken: " + details
	p.PeerLock.Unlock()

	p.DataLock.Lock()
	// clear data except the status table which is required to see if the backend has been restarted
	for name := range p.Tables {
		if name != "status" {
			table := p.Tables[name]
			table.Data = make([][]interface{}, 0)
			table.Refs = make(map[string][][]interface{})
			table.Index = make(map[string][]interface{})
		}
	}
	p.DataLock.Unlock()
}

func logPanicExitPeer(p *Peer) {
	if r := recover(); r != nil {
		log.Errorf("[%s] Panic: %s", p.Name, r)
		log.Errorf("[%s] %s", p.Name, debug.Stack())
		if p.lastRequest != nil {
			log.Errorf("[%s] LastQuery:", p.Name)
			log.Errorf("[%s] %s", p.Name, p.lastRequest.String())
			log.Errorf("[%s] LastResponse:", p.Name)
			log.Errorf("[%s] %s", p.Name, string(*(p.lastResponse)))
		}
		os.Exit(1)
	}
}
