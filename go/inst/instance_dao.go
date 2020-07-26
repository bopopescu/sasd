/*
   Copyright 2014 Outbrain Inc.

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

package inst

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openark/golib/log"
	"github.com/openark/golib/math"
	"github.com/openark/golib/sqlutils"
	"github.com/patrickmn/go-cache"
	"github.com/rcrowley/go-metrics"
	"github.com/sjmudd/stopwatch"

	"github.com/github/orchestrator/go/attributes"
	"github.com/github/orchestrator/go/collection"
	"github.com/github/orchestrator/go/config"
	"github.com/github/orchestrator/go/db"
	"github.com/github/orchestrator/go/kv"
	"github.com/github/orchestrator/go/metrics/query"
)

const (
	backendDBConcurrency   = 20
	error1045AccessDenied  = "Error 1045: Access denied for user"
	errorConnectionRefused = "getsockopt: connection refused"
	errorNoSuchHost        = "no such host"
	errorIOTimeout         = "i/o timeout"
)

var instanceReadChan = make(chan bool, backendDBConcurrency)
var instanceWriteChan = make(chan bool, backendDBConcurrency)

// InstancesByCountSubordinateHosts is a sortable type for Instance
type InstancesByCountSubordinateHosts [](*Instance)

func (this InstancesByCountSubordinateHosts) Len() int      { return len(this) }
func (this InstancesByCountSubordinateHosts) Swap(i, j int) { this[i], this[j] = this[j], this[i] }
func (this InstancesByCountSubordinateHosts) Less(i, j int) bool {
	return len(this[i].SubordinateHosts) < len(this[j].SubordinateHosts)
}

// instanceKeyInformativeClusterName is a non-authoritative cache; used for auditing or general purpose.
var instanceKeyInformativeClusterName *cache.Cache
var forgetInstanceKeys *cache.Cache
var clusterInjectedPseudoGTIDCache *cache.Cache

var accessDeniedCounter = metrics.NewCounter()
var readTopologyInstanceCounter = metrics.NewCounter()
var readInstanceCounter = metrics.NewCounter()
var writeInstanceCounter = metrics.NewCounter()
var backendWrites = collection.CreateOrReturnCollection("BACKEND_WRITES")

func init() {
	metrics.Register("instance.access_denied", accessDeniedCounter)
	metrics.Register("instance.read_topology", readTopologyInstanceCounter)
	metrics.Register("instance.read", readInstanceCounter)
	metrics.Register("instance.write", writeInstanceCounter)

	go initializeInstanceDao()
}

func initializeInstanceDao() {
	config.WaitForConfigurationToBeLoaded()
	instanceWriteBuffer = make(chan instanceUpdateObject, config.Config.InstanceWriteBufferSize)
	instanceKeyInformativeClusterName = cache.New(time.Duration(config.Config.InstancePollSeconds/2)*time.Second, time.Second)
	forgetInstanceKeys = cache.New(time.Duration(config.Config.InstancePollSeconds*3)*time.Second, time.Second)
	clusterInjectedPseudoGTIDCache = cache.New(time.Minute, time.Second)
	// spin off instance write buffer flushing
	go func() {
		flushTick := time.Tick(time.Duration(config.Config.InstanceFlushIntervalMilliseconds) * time.Millisecond)
		for {
			// it is time to flush
			select {
			case <-flushTick:
				flushInstanceWriteBuffer()
			case <-forceFlushInstanceWriteBuffer:
				flushInstanceWriteBuffer()
			}
		}
	}()
}

// ExecDBWriteFunc chooses how to execute a write onto the database: whether synchronuously or not
func ExecDBWriteFunc(f func() error) error {
	m := query.NewMetric()

	instanceWriteChan <- true
	m.WaitLatency = time.Since(m.Timestamp)

	// catch the exec time and error if there is one
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(runtime.Error); ok {
				panic(r)
			}

			if s, ok := r.(string); ok {
				m.Err = errors.New(s)
			} else {
				m.Err = r.(error)
			}
		}
		m.ExecuteLatency = time.Since(m.Timestamp.Add(m.WaitLatency))
		backendWrites.Append(m)
		<-instanceWriteChan // assume this takes no time
	}()
	res := f()
	return res
}

func ExpireTableData(tableName string, timestampColumn string) error {
	query := fmt.Sprintf("delete from %s where %s < NOW() - INTERVAL ? DAY", tableName, timestampColumn)
	writeFunc := func() error {
		_, err := db.ExecOrchestrator(query, config.AuditPurgeDays)
		return err
	}
	return ExecDBWriteFunc(writeFunc)
}

// logReadTopologyInstanceError logs an error, if applicable, for a ReadTopologyInstance operation,
// providing context and hint as for the source of the error. If there's no hint just provide the
// original error.
func logReadTopologyInstanceError(instanceKey *InstanceKey, hint string, err error) error {
	if err == nil {
		return nil
	}
	var msg string
	if hint == "" {
		msg = fmt.Sprintf("ReadTopologyInstance(%+v): %+v", *instanceKey, err)
	} else {
		msg = fmt.Sprintf("ReadTopologyInstance(%+v) %+v: %+v",
			*instanceKey,
			strings.Replace(hint, "%", "%%", -1), // escape %
			err)
	}
	return log.Errorf(msg)
}

// ReadTopologyInstance collects information on the state of a MySQL
// server and writes the result synchronously to the orchestrator
// backend.
func ReadTopologyInstance(instanceKey *InstanceKey) (*Instance, error) {
	return ReadTopologyInstanceBufferable(instanceKey, false, nil)
}

// Is this an error which means that we shouldn't try going more queries for this discovery attempt?
func unrecoverableError(err error) bool {
	contains := []string{
		error1045AccessDenied,
		errorConnectionRefused,
		errorIOTimeout,
		errorNoSuchHost,
	}
	for _, k := range contains {
		if strings.Contains(err.Error(), k) {
			return true
		}
	}
	return false
}

// Check if the instance is a MaxScale binlog server (a proxy not a real
// MySQL server) and also update the resolved hostname
func (instance *Instance) checkMaxScale(db *sql.DB, latency *stopwatch.NamedStopwatch) (isMaxScale bool, resolvedHostname string, err error) {
	if config.Config.SkipMaxScaleCheck {
		return isMaxScale, resolvedHostname, err
	}

	latency.Start("instance")
	err = sqlutils.QueryRowsMap(db, "show variables like 'maxscale%'", func(m sqlutils.RowMap) error {
		if m.GetString("Variable_name") == "MAXSCALE_VERSION" {
			originalVersion := m.GetString("Value")
			if originalVersion == "" {
				originalVersion = m.GetString("value")
			}
			if originalVersion == "" {
				originalVersion = "0.0.0"
			}
			instance.Version = originalVersion + "-maxscale"
			instance.ServerID = 0
			instance.ServerUUID = ""
			instance.Uptime = 0
			instance.Binlog_format = "INHERIT"
			instance.ReadOnly = true
			instance.LogBinEnabled = true
			instance.LogSubordinateUpdatesEnabled = true
			resolvedHostname = instance.Key.Hostname
			latency.Start("backend")
			UpdateResolvedHostname(resolvedHostname, resolvedHostname)
			latency.Stop("backend")
			isMaxScale = true
		}
		return nil
	})
	latency.Stop("instance")

	// Detect failed connection attempts and don't report the command
	// we are executing as that might be confusing.
	if err != nil {
		if strings.Contains(err.Error(), error1045AccessDenied) {
			accessDeniedCounter.Inc(1)
		}
		if unrecoverableError(err) {
			logReadTopologyInstanceError(&instance.Key, "", err)
		} else {
			logReadTopologyInstanceError(&instance.Key, "show variables like 'maxscale%'", err)
		}
	}

	return isMaxScale, resolvedHostname, err
}

// AreReplicationThreadsRunning checks if both IO and SQL threads are running
func AreReplicationThreadsRunning(instanceKey *InstanceKey) (replicationThreadsRunning bool, err error) {
	db, err := db.OpenTopology(instanceKey.Hostname, instanceKey.Port)
	if err != nil {
		return replicationThreadsRunning, err
	}
	err = sqlutils.QueryRowsMap(db, "show subordinate status", func(m sqlutils.RowMap) error {
		ioThreadRunning := (m.GetString("Subordinate_IO_Running") == "Yes")
		sqlThreadRunning := (m.GetString("Subordinate_SQL_Running") == "Yes")
		replicationThreadsRunning = ioThreadRunning && sqlThreadRunning
		return nil
	})
	return replicationThreadsRunning, err
}

// ReadTopologyInstanceBufferable connects to a topology MySQL instance
// and collects information on the server and its replication state.
// It writes the information retrieved into orchestrator's backend.
// - writes are optionally buffered.
// - timing information can be collected for the stages performed.
func ReadTopologyInstanceBufferable(instanceKey *InstanceKey, bufferWrites bool, latency *stopwatch.NamedStopwatch) (*Instance, error) {
	defer func() {
		if err := recover(); err != nil {
			logReadTopologyInstanceError(instanceKey, "Unexpected, aborting", fmt.Errorf("%+v", err))
		}
	}()

	var waitGroup sync.WaitGroup
	readingStartTime := time.Now()
	instance := NewInstance()
	instanceFound := false
	partialSuccess := false
	foundByShowSubordinateHosts := false
	longRunningProcesses := []Process{}
	resolvedHostname := ""
	maxScaleMainHostname := ""
	isMaxScale := false
	isMaxScale110 := false
	subordinateStatusFound := false
	var resolveErr error

	if !instanceKey.IsValid() {
		latency.Start("backend")
		if err := UpdateInstanceLastAttemptedCheck(instanceKey); err != nil {
			log.Errorf("ReadTopologyInstanceBufferable: %+v: %v", instanceKey, err)
		}
		latency.Stop("backend")
		return instance, fmt.Errorf("ReadTopologyInstance will not act on invalid instance key: %+v", *instanceKey)
	}

	lastAttemptedCheckTimer := time.AfterFunc(time.Second, func() {
		go UpdateInstanceLastAttemptedCheck(instanceKey)
	})

	latency.Start("instance")
	db, err := db.OpenDiscovery(instanceKey.Hostname, instanceKey.Port)
	latency.Stop("instance")
	if err != nil {
		goto Cleanup
	}

	instance.Key = *instanceKey

	if isMaxScale, resolvedHostname, err = instance.checkMaxScale(db, latency); err != nil {
		// We do not "goto Cleanup" here, although it should be the correct flow.
		// Reason is 5.7's new security feature that requires GRANTs on performance_schema.session_variables.
		// There is a wrong decision making in this design and the migration path to 5.7 will be difficult.
		// I don't want orchestrator to put even more burden on this.
		// If the statement errors, then we are unable to determine that this is maxscale, hence assume it is not.
		// In which case there would be other queries sent to the server that are not affected by 5.7 behavior, and that will fail.

		// Certain errors are not recoverable (for this discovery process) so it's fine to go to Cleanup
		if unrecoverableError(err) {
			goto Cleanup
		}
	}

	latency.Start("instance")
	if isMaxScale {
		if strings.Contains(instance.Version, "1.1.0") {
			isMaxScale110 = true

			// Buggy buggy maxscale 1.1.0. Reported Main_Host can be corrupted.
			// Therefore we (currently) take @@hostname (which is masquerading as main host anyhow)
			err = db.QueryRow("select @@hostname").Scan(&maxScaleMainHostname)
			if err != nil {
				goto Cleanup
			}
		}
		if isMaxScale110 {
			// Only this is supported:
			db.QueryRow("select @@server_id").Scan(&instance.ServerID)
		} else {
			db.QueryRow("select @@global.server_id").Scan(&instance.ServerID)
			db.QueryRow("select @@global.server_uuid").Scan(&instance.ServerUUID)
		}
	} else {
		// NOT MaxScale

		// We begin with a few operations we can run concurrently, and which do not depend on anything
		{
			waitGroup.Add(1)
			go func() {
				defer waitGroup.Done()
				var dummy string
				// show global status works just as well with 5.6 & 5.7 (5.7 moves variables to performance_schema)
				err = db.QueryRow("show global status like 'Uptime'").Scan(&dummy, &instance.Uptime)

				if err != nil {
					logReadTopologyInstanceError(instanceKey, "show global status like 'Uptime'", err)

					// We do not "goto Cleanup" here, although it should be the correct flow.
					// Reason is 5.7's new security feature that requires GRANTs on performance_schema.global_variables.
					// There is a wrong decisionmaking in this design and the migration path to 5.7 will be difficult.
					// I don't want orchestrator to put even more burden on this. The 'Uptime' variable is not that important
					// so as to completely fail reading a 5.7 instance.
					// This is supposed to be fixed in 5.7.9
				}
			}()
		}

		var mysqlHostname, mysqlReportHost string
		err = db.QueryRow("select @@global.hostname, ifnull(@@global.report_host, ''), @@global.server_id, @@global.version, @@global.version_comment, @@global.read_only, @@global.binlog_format, @@global.log_bin, @@global.log_subordinate_updates").Scan(
			&mysqlHostname, &mysqlReportHost, &instance.ServerID, &instance.Version, &instance.VersionComment, &instance.ReadOnly, &instance.Binlog_format, &instance.LogBinEnabled, &instance.LogSubordinateUpdatesEnabled)
		if err != nil {
			goto Cleanup
		}
		partialSuccess = true // We at least managed to read something from the server.
		switch strings.ToLower(config.Config.MySQLHostnameResolveMethod) {
		case "none":
			resolvedHostname = instance.Key.Hostname
		case "default", "hostname", "@@hostname":
			resolvedHostname = mysqlHostname
		case "report_host", "@@report_host":
			if mysqlReportHost == "" {
				err = fmt.Errorf("MySQLHostnameResolveMethod configured to use @@report_host but %+v has NULL/empty @@report_host", instanceKey)
				goto Cleanup
			}
			resolvedHostname = mysqlReportHost
		default:
			resolvedHostname = instance.Key.Hostname
		}

		if instance.LogBinEnabled {
			waitGroup.Add(1)
			go func() {
				defer waitGroup.Done()
				err = sqlutils.QueryRowsMap(db, "show main status", func(m sqlutils.RowMap) error {
					var err error
					instance.SelfBinlogCoordinates.LogFile = m.GetString("File")
					instance.SelfBinlogCoordinates.LogPos = m.GetInt64("Position")
					return err
				})
			}()
		}

		{
			waitGroup.Add(1)
			go func() {
				defer waitGroup.Done()
				err = sqlutils.QueryRowsMap(db, "show global status like 'rpl_semi_sync_%_status'", func(m sqlutils.RowMap) error {
					if m.GetString("Variable_name") == "Rpl_semi_sync_main_status" {
						instance.SemiSyncMainEnabled = (m.GetString("Value") == "ON")
					} else if m.GetString("Variable_name") == "Rpl_semi_sync_subordinate_status" {
						instance.SemiSyncReplicaEnabled = (m.GetString("Value") == "ON")
					}
					return nil
				})
			}()
		}
		if (instance.IsOracleMySQL() || instance.IsPercona()) && !instance.IsSmallerMajorVersionByString("5.6") {
			waitGroup.Add(1)
			go func() {
				defer waitGroup.Done()
				var mainInfoRepositoryOnTable bool
				// Stuff only supported on Oracle MySQL >= 5.6
				// ...
				// @@gtid_mode only available in Orcale MySQL >= 5.6
				// Previous version just issued this query brute-force, but I don't like errors being issued where they shouldn't.
				_ = db.QueryRow("select @@global.gtid_mode, @@global.server_uuid, @@global.gtid_purged, @@global.main_info_repository = 'TABLE', @@global.binlog_row_image").Scan(&instance.GTIDMode, &instance.ServerUUID, &instance.GtidPurged, &mainInfoRepositoryOnTable, &instance.BinlogRowImage)
				if instance.GTIDMode != "" && instance.GTIDMode != "OFF" {
					instance.SupportsOracleGTID = true
				}
				if mainInfoRepositoryOnTable {
					_ = db.QueryRow("select count(*) > 0 and MAX(User_name) != '' from mysql.subordinate_main_info").Scan(&instance.ReplicationCredentialsAvailable)
				}
			}()
		}
	}
	if resolvedHostname != instance.Key.Hostname {
		latency.Start("backend")
		UpdateResolvedHostname(instance.Key.Hostname, resolvedHostname)
		latency.Stop("backend")
		instance.Key.Hostname = resolvedHostname
	}
	if instance.Key.Hostname == "" {
		err = fmt.Errorf("ReadTopologyInstance: empty hostname (%+v). Bailing out", *instanceKey)
		goto Cleanup
	}
	go ResolveHostnameIPs(instance.Key.Hostname)
	if config.Config.DataCenterPattern != "" {
		if pattern, err := regexp.Compile(config.Config.DataCenterPattern); err == nil {
			match := pattern.FindStringSubmatch(instance.Key.Hostname)
			if len(match) != 0 {
				instance.DataCenter = match[1]
			}
		}
		// This can be overriden by later invocation of DetectDataCenterQuery
	}
	if config.Config.PhysicalEnvironmentPattern != "" {
		if pattern, err := regexp.Compile(config.Config.PhysicalEnvironmentPattern); err == nil {
			match := pattern.FindStringSubmatch(instance.Key.Hostname)
			if len(match) != 0 {
				instance.PhysicalEnvironment = match[1]
			}
		}
		// This can be overriden by later invocation of DetectPhysicalEnvironmentQuery
	}

	err = sqlutils.QueryRowsMap(db, "show subordinate status", func(m sqlutils.RowMap) error {
		instance.HasReplicationCredentials = (m.GetString("Main_User") != "")
		instance.Subordinate_IO_Running = (m.GetString("Subordinate_IO_Running") == "Yes")
		if isMaxScale110 {
			// Covering buggy MaxScale 1.1.0
			instance.Subordinate_IO_Running = instance.Subordinate_IO_Running && (m.GetString("Subordinate_IO_State") == "Binlog Dump")
		}
		instance.Subordinate_SQL_Running = (m.GetString("Subordinate_SQL_Running") == "Yes")
		instance.ReadBinlogCoordinates.LogFile = m.GetString("Main_Log_File")
		instance.ReadBinlogCoordinates.LogPos = m.GetInt64("Read_Main_Log_Pos")
		instance.ExecBinlogCoordinates.LogFile = m.GetString("Relay_Main_Log_File")
		instance.ExecBinlogCoordinates.LogPos = m.GetInt64("Exec_Main_Log_Pos")
		instance.IsDetached, _ = instance.ExecBinlogCoordinates.ExtractDetachedCoordinates()
		instance.RelaylogCoordinates.LogFile = m.GetString("Relay_Log_File")
		instance.RelaylogCoordinates.LogPos = m.GetInt64("Relay_Log_Pos")
		instance.RelaylogCoordinates.Type = RelayLog
		instance.LastSQLError = strconv.QuoteToASCII(m.GetString("Last_SQL_Error"))
		instance.LastIOError = strconv.QuoteToASCII(m.GetString("Last_IO_Error"))
		instance.SQLDelay = m.GetUintD("SQL_Delay", 0)
		instance.UsingOracleGTID = (m.GetIntD("Auto_Position", 0) == 1)
		instance.ExecutedGtidSet = m.GetStringD("Executed_Gtid_Set", "")
		instance.UsingMariaDBGTID = (m.GetStringD("Using_Gtid", "No") != "No")
		instance.HasReplicationFilters = ((m.GetStringD("Replicate_Do_DB", "") != "") || (m.GetStringD("Replicate_Ignore_DB", "") != "") || (m.GetStringD("Replicate_Do_Table", "") != "") || (m.GetStringD("Replicate_Ignore_Table", "") != "") || (m.GetStringD("Replicate_Wild_Do_Table", "") != "") || (m.GetStringD("Replicate_Wild_Ignore_Table", "") != ""))

		mainHostname := m.GetString("Main_Host")
		if isMaxScale110 {
			// Buggy buggy maxscale 1.1.0. Reported Main_Host can be corrupted.
			// Therefore we (currently) take @@hostname (which is masquarading as main host anyhow)
			mainHostname = maxScaleMainHostname
		}
		mainKey, err := NewInstanceKeyFromStrings(mainHostname, m.GetString("Main_Port"))
		if err != nil {
			logReadTopologyInstanceError(instanceKey, "NewInstanceKeyFromStrings", err)
		}
		mainKey.Hostname, resolveErr = ResolveHostname(mainKey.Hostname)
		if resolveErr != nil {
			logReadTopologyInstanceError(instanceKey, fmt.Sprintf("ResolveHostname(%q)", mainKey.Hostname), resolveErr)
		}
		instance.MainKey = *mainKey
		instance.IsDetachedMain = instance.MainKey.IsDetached()
		instance.SecondsBehindMain = m.GetNullInt64("Seconds_Behind_Main")
		if instance.SecondsBehindMain.Valid && instance.SecondsBehindMain.Int64 < 0 {
			log.Warningf("Host: %+v, instance.SecondsBehindMain < 0 [%+v], correcting to 0", instanceKey, instance.SecondsBehindMain.Int64)
			instance.SecondsBehindMain.Int64 = 0
		}
		// And until told otherwise:
		instance.SubordinateLagSeconds = instance.SecondsBehindMain

		instance.AllowTLS = (m.GetString("Main_SSL_Allowed") == "Yes")
		// Not breaking the flow even on error
		subordinateStatusFound = true
		return nil
	})
	if err != nil {
		goto Cleanup
	}
	if isMaxScale && !subordinateStatusFound {
		err = fmt.Errorf("No 'SHOW SLAVE STATUS' output found for a MaxScale instance: %+v", instanceKey)
		goto Cleanup
	}

	if config.Config.ReplicationLagQuery != "" && !isMaxScale {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			if err := db.QueryRow(config.Config.ReplicationLagQuery).Scan(&instance.SubordinateLagSeconds); err == nil {
				if instance.SubordinateLagSeconds.Valid && instance.SubordinateLagSeconds.Int64 < 0 {
					log.Warningf("Host: %+v, instance.SubordinateLagSeconds < 0 [%+v], correcting to 0", instanceKey, instance.SubordinateLagSeconds.Int64)
					instance.SubordinateLagSeconds.Int64 = 0
				}
			} else {
				instance.SubordinateLagSeconds = instance.SecondsBehindMain
				logReadTopologyInstanceError(instanceKey, "ReplicationLagQuery", err)
			}
		}()
	}

	instanceFound = true

	// -------------------------------------------------------------------------
	// Anything after this point does not affect the fact the instance is found.
	// No `goto Cleanup` after this point.
	// -------------------------------------------------------------------------

	// Get replicas, either by SHOW SLAVE HOSTS or via PROCESSLIST
	// MaxScale does not support PROCESSLIST, so SHOW SLAVE HOSTS is the only option
	if config.Config.DiscoverByShowSubordinateHosts || isMaxScale {
		err := sqlutils.QueryRowsMap(db, `show subordinate hosts`,
			func(m sqlutils.RowMap) error {
				// MaxScale 1.1 may trigger an error with this command, but
				// also we may see issues if anything on the MySQL server locks up.
				// Consequently it's important to validate the values received look
				// good prior to calling ResolveHostname()
				host := m.GetString("Host")
				port := m.GetString("Port")
				if host == "" || port == "" {
					if isMaxScale && host == "" && port == "0" {
						// MaxScale reports a bad response sometimes so ignore it.
						// - seen in 1.1.0 and 1.4.3.4
						return nil
					}
					// otherwise report the error to the caller
					return fmt.Errorf("ReadTopologyInstance(%+v) 'show subordinate hosts' returned row with <host,port>: <%v,%v>", instanceKey, host, port)
				}

				// Note: NewInstanceKeyFromStrings calls ResolveHostname() implicitly
				replicaKey, err := NewInstanceKeyFromStrings(host, port)
				if err == nil && replicaKey.IsValid() {
					instance.AddReplicaKey(replicaKey)
					foundByShowSubordinateHosts = true
				}
				return err
			})

		logReadTopologyInstanceError(instanceKey, "show subordinate hosts", err)
	}
	if !foundByShowSubordinateHosts && !isMaxScale {
		// Either not configured to read SHOW SLAVE HOSTS or nothing was there.
		// Discover by information_schema.processlist
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			err := sqlutils.QueryRowsMap(db, `
      	select
      		substring_index(host, ':', 1) as subordinate_hostname
      	from
      		information_schema.processlist
      	where
          command IN ('Binlog Dump', 'Binlog Dump GTID')
  		`,
				func(m sqlutils.RowMap) error {
					cname, resolveErr := ResolveHostname(m.GetString("subordinate_hostname"))
					if resolveErr != nil {
						logReadTopologyInstanceError(instanceKey, "ResolveHostname: processlist", resolveErr)
					}
					replicaKey := InstanceKey{Hostname: cname, Port: instance.Key.Port}
					instance.AddReplicaKey(&replicaKey)
					return err
				})

			logReadTopologyInstanceError(instanceKey, "processlist", err)
		}()
	}

	if config.Config.DetectDataCenterQuery != "" && !isMaxScale {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			err := db.QueryRow(config.Config.DetectDataCenterQuery).Scan(&instance.DataCenter)
			logReadTopologyInstanceError(instanceKey, "DetectDataCenterQuery", err)
		}()
	}

	if config.Config.DetectPhysicalEnvironmentQuery != "" && !isMaxScale {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			err := db.QueryRow(config.Config.DetectPhysicalEnvironmentQuery).Scan(&instance.PhysicalEnvironment)
			logReadTopologyInstanceError(instanceKey, "DetectPhysicalEnvironmentQuery", err)
		}()
	}

	if config.Config.DetectInstanceAliasQuery != "" && !isMaxScale {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			err := db.QueryRow(config.Config.DetectInstanceAliasQuery).Scan(&instance.InstanceAlias)
			logReadTopologyInstanceError(instanceKey, "DetectInstanceAliasQuery", err)
		}()
	}

	if config.Config.DetectSemiSyncEnforcedQuery != "" && !isMaxScale {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			err := db.QueryRow(config.Config.DetectSemiSyncEnforcedQuery).Scan(&instance.SemiSyncEnforced)
			logReadTopologyInstanceError(instanceKey, "DetectSemiSyncEnforcedQuery", err)
		}()
	}

	{
		latency.Start("backend")
		err = ReadInstanceClusterAttributes(instance)
		latency.Stop("backend")
		logReadTopologyInstanceError(instanceKey, "ReadInstanceClusterAttributes", err)
	}

	{
		// Pseudo GTID
		// Depends on ReadInstanceClusterAttributes above
		instance.UsingPseudoGTID = false
		if config.Config.AutoPseudoGTID {
			var err error
			instance.UsingPseudoGTID, err = isInjectedPseudoGTID(instance.ClusterName)
			log.Errore(err)
		} else if config.Config.DetectPseudoGTIDQuery != "" {
			waitGroup.Add(1)
			go func() {
				defer waitGroup.Done()
				if resultData, err := sqlutils.QueryResultData(db, config.Config.DetectPseudoGTIDQuery); err == nil {
					if len(resultData) > 0 {
						if len(resultData[0]) > 0 {
							if resultData[0][0].Valid && resultData[0][0].String == "1" {
								instance.UsingPseudoGTID = true
							}
						}
					}
				} else {
					logReadTopologyInstanceError(instanceKey, "DetectPseudoGTIDQuery", err)
				}
			}()
		}
	}

	// First read the current PromotionRule from candidate_database_instance.
	{
		latency.Start("backend")
		err = ReadInstancePromotionRule(instance)
		latency.Stop("backend")
		logReadTopologyInstanceError(instanceKey, "ReadInstancePromotionRule", err)
	}
	// Then check if the instance wants to set a different PromotionRule.
	// We'll set it here on their behalf so there's no race between the first
	// time an instance is discovered, and setting a rule like "must_not".
	if config.Config.DetectPromotionRuleQuery != "" && !isMaxScale {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			var value string
			err := db.QueryRow(config.Config.DetectPromotionRuleQuery).Scan(&value)
			logReadTopologyInstanceError(instanceKey, "DetectPromotionRuleQuery", err)
			promotionRule, err := ParseCandidatePromotionRule(value)
			logReadTopologyInstanceError(instanceKey, "ParseCandidatePromotionRule", err)
			if err == nil {
				// We need to update candidate_database_instance.
				// We register the rule even if it hasn't changed,
				// to bump the last_suggested time.
				instance.PromotionRule = promotionRule
				err = RegisterCandidateInstance(NewCandidateDatabaseInstance(instanceKey, promotionRule))
				logReadTopologyInstanceError(instanceKey, "RegisterCandidateInstance", err)
			}
		}()
	}

	ReadClusterAliasOverride(instance)
	if !isMaxScale {
		if instance.SuggestedClusterAlias == "" {
			// Only need to do on mains
			if config.Config.DetectClusterAliasQuery != "" {
				clusterAlias := ""
				if err := db.QueryRow(config.Config.DetectClusterAliasQuery).Scan(&clusterAlias); err != nil {
					logReadTopologyInstanceError(instanceKey, "DetectClusterAliasQuery", err)
				} else {
					instance.SuggestedClusterAlias = clusterAlias
				}
			}
		}
		if instance.SuggestedClusterAlias == "" {
			// Not found by DetectClusterAliasQuery...
			// See if a ClusterNameToAlias configuration applies
			if clusterAlias := mappedClusterNameToAlias(instance.ClusterName); clusterAlias != "" {
				instance.SuggestedClusterAlias = clusterAlias
			}
		}
	}
	if instance.ReplicationDepth == 0 && config.Config.DetectClusterDomainQuery != "" && !isMaxScale {
		// Only need to do on mains
		domainName := ""
		if err := db.QueryRow(config.Config.DetectClusterDomainQuery).Scan(&domainName); err != nil {
			domainName = ""
			logReadTopologyInstanceError(instanceKey, "DetectClusterDomainQuery", err)
		}
		if domainName != "" {
			latency.Start("backend")
			err := WriteClusterDomainName(instance.ClusterName, domainName)
			latency.Stop("backend")
			logReadTopologyInstanceError(instanceKey, "WriteClusterDomainName", err)
		}
	}

Cleanup:
	waitGroup.Wait()
	latency.Stop("instance")
	readTopologyInstanceCounter.Inc(1)
	//	logReadTopologyInstanceError(instanceKey, "ReadTopologyInstanceBufferable", err)	// don't write here and a few lines later.
	if instanceFound {
		instance.LastDiscoveryLatency = time.Since(readingStartTime)
		instance.IsLastCheckValid = true
		instance.IsRecentlyChecked = true
		instance.IsUpToDate = true
		latency.Start("backend")
		if bufferWrites {
			enqueueInstanceWrite(instance, instanceFound, err)
		} else {
			WriteInstance(instance, instanceFound, err)
		}
		WriteLongRunningProcesses(&instance.Key, longRunningProcesses)
		lastAttemptedCheckTimer.Stop()
		latency.Stop("backend")
		return instance, nil
	}

	// Something is wrong, could be network-wise. Record that we
	// tried to check the instance. last_attempted_check is also
	// updated on success by writeInstance.
	latency.Start("backend")
	_ = UpdateInstanceLastChecked(&instance.Key, partialSuccess)
	latency.Stop("backend")
	return nil, err
}

// ReadClusterAliasOverride reads and applies SuggestedClusterAlias based on cluster_alias_override
func ReadClusterAliasOverride(instance *Instance) (err error) {
	aliasOverride := ""
	query := `
		select
			alias
		from
			cluster_alias_override
		where
			cluster_name = ?
			`
	err = db.QueryOrchestrator(query, sqlutils.Args(instance.ClusterName), func(m sqlutils.RowMap) error {
		aliasOverride = m.GetString("alias")

		return nil
	})
	if aliasOverride != "" {
		instance.SuggestedClusterAlias = aliasOverride
	}
	return err
}

// ReadInstanceClusterAttributes will return the cluster name for a given instance by looking at its main
// and getting it from there.
// It is a non-recursive function and so-called-recursion is performed upon periodic reading of
// instances.
func ReadInstanceClusterAttributes(instance *Instance) (err error) {
	var mainMainKey InstanceKey
	var mainClusterName string
	var mainSuggestedClusterAlias string
	var mainReplicationDepth uint
	mainDataFound := false

	// Read the cluster_name of the _main_ of our instance, derive it from there.
	query := `
			select
					cluster_name,
					suggested_cluster_alias,
					replication_depth,
					main_host,
					main_port
				from database_instance
				where hostname=? and port=?
	`
	args := sqlutils.Args(instance.MainKey.Hostname, instance.MainKey.Port)

	err = db.QueryOrchestrator(query, args, func(m sqlutils.RowMap) error {
		mainClusterName = m.GetString("cluster_name")
		mainSuggestedClusterAlias = m.GetString("suggested_cluster_alias")
		mainReplicationDepth = m.GetUint("replication_depth")
		mainMainKey.Hostname = m.GetString("main_host")
		mainMainKey.Port = m.GetInt("main_port")
		mainDataFound = true
		return nil
	})
	if err != nil {
		return log.Errore(err)
	}

	var replicationDepth uint = 0
	var clusterName string
	if mainDataFound {
		replicationDepth = mainReplicationDepth + 1
		clusterName = mainClusterName
	}
	clusterNameByInstanceKey := instance.Key.StringCode()
	if clusterName == "" {
		// Nothing from main; we set it to be named after the instance itself
		clusterName = clusterNameByInstanceKey
	}

	isCoMain := false
	if mainMainKey.Equals(&instance.Key) {
		// co-main calls for special case, in fear of the infinite loop
		isCoMain = true
		clusterNameByCoMainKey := instance.MainKey.StringCode()
		if clusterName != clusterNameByInstanceKey && clusterName != clusterNameByCoMainKey {
			// Can be caused by a co-main topology failover
			log.Errorf("ReadInstanceClusterAttributes: in co-main topology %s is not in (%s, %s). Forcing it to become one of them", clusterName, clusterNameByInstanceKey, clusterNameByCoMainKey)
			clusterName = math.TernaryString(instance.Key.SmallerThan(&instance.MainKey), clusterNameByInstanceKey, clusterNameByCoMainKey)
		}
		if clusterName == clusterNameByInstanceKey {
			// circular replication. Avoid infinite ++ on replicationDepth
			replicationDepth = 0
		} // While the other stays "1"
	}
	instance.ClusterName = clusterName
	instance.SuggestedClusterAlias = mainSuggestedClusterAlias
	instance.ReplicationDepth = replicationDepth
	instance.IsCoMain = isCoMain
	return nil
}

type byNamePort [](*InstanceKey)

func (this byNamePort) Len() int      { return len(this) }
func (this byNamePort) Swap(i, j int) { this[i], this[j] = this[j], this[i] }
func (this byNamePort) Less(i, j int) bool {
	return (this[i].Hostname < this[j].Hostname) ||
		(this[i].Hostname == this[j].Hostname && this[i].Port < this[j].Port)
}

// BulkReadInstance returns a list of all instances from the database
// - I only need the Hostname and Port fields.
// - I must use readInstancesByCondition to ensure all column
//   settings are correct.
func BulkReadInstance() ([](*InstanceKey), error) {
	// no condition (I want all rows) and no sorting (but this is done by Hostname, Port anyway)
	const (
		condition = "1=1"
		orderBy   = ""
	)
	var instanceKeys [](*InstanceKey)

	instances, err := readInstancesByCondition(condition, nil, orderBy)
	if err != nil {
		return nil, fmt.Errorf("BulkReadInstance: %+v", err)
	}

	// update counters if we picked anything up
	if len(instances) > 0 {
		readInstanceCounter.Inc(int64(len(instances)))

		for _, instance := range instances {
			instanceKeys = append(instanceKeys, &instance.Key)
		}
		// sort on orchestrator and not the backend (should be redundant)
		sort.Sort(byNamePort(instanceKeys))
	}

	return instanceKeys, nil
}

func ReadInstancePromotionRule(instance *Instance) (err error) {
	var promotionRule CandidatePromotionRule = NeutralPromoteRule
	query := `
			select
				ifnull(nullif(promotion_rule, ''), 'neutral') as promotion_rule
				from candidate_database_instance
				where hostname=? and port=?
	`
	args := sqlutils.Args(instance.Key.Hostname, instance.Key.Port)

	err = db.QueryOrchestrator(query, args, func(m sqlutils.RowMap) error {
		promotionRule = CandidatePromotionRule(m.GetString("promotion_rule"))
		return nil
	})
	instance.PromotionRule = promotionRule
	return log.Errore(err)
}

// readInstanceRow reads a single instance row from the orchestrator backend database.
func readInstanceRow(m sqlutils.RowMap) *Instance {
	instance := NewInstance()

	instance.Key.Hostname = m.GetString("hostname")
	instance.Key.Port = m.GetInt("port")
	instance.Uptime = m.GetUint("uptime")
	instance.ServerID = m.GetUint("server_id")
	instance.ServerUUID = m.GetString("server_uuid")
	instance.Version = m.GetString("version")
	instance.VersionComment = m.GetString("version_comment")
	instance.ReadOnly = m.GetBool("read_only")
	instance.Binlog_format = m.GetString("binlog_format")
	instance.BinlogRowImage = m.GetString("binlog_row_image")
	instance.LogBinEnabled = m.GetBool("log_bin")
	instance.LogSubordinateUpdatesEnabled = m.GetBool("log_subordinate_updates")
	instance.MainKey.Hostname = m.GetString("main_host")
	instance.MainKey.Port = m.GetInt("main_port")
	instance.IsDetachedMain = instance.MainKey.IsDetached()
	instance.Subordinate_SQL_Running = m.GetBool("subordinate_sql_running")
	instance.Subordinate_IO_Running = m.GetBool("subordinate_io_running")
	instance.HasReplicationFilters = m.GetBool("has_replication_filters")
	instance.SupportsOracleGTID = m.GetBool("supports_oracle_gtid")
	instance.UsingOracleGTID = m.GetBool("oracle_gtid")
	instance.ExecutedGtidSet = m.GetString("executed_gtid_set")
	instance.GTIDMode = m.GetString("gtid_mode")
	instance.GtidPurged = m.GetString("gtid_purged")
	instance.UsingMariaDBGTID = m.GetBool("mariadb_gtid")
	instance.UsingPseudoGTID = m.GetBool("pseudo_gtid")
	instance.SelfBinlogCoordinates.LogFile = m.GetString("binary_log_file")
	instance.SelfBinlogCoordinates.LogPos = m.GetInt64("binary_log_pos")
	instance.ReadBinlogCoordinates.LogFile = m.GetString("main_log_file")
	instance.ReadBinlogCoordinates.LogPos = m.GetInt64("read_main_log_pos")
	instance.ExecBinlogCoordinates.LogFile = m.GetString("relay_main_log_file")
	instance.ExecBinlogCoordinates.LogPos = m.GetInt64("exec_main_log_pos")
	instance.IsDetached, _ = instance.ExecBinlogCoordinates.ExtractDetachedCoordinates()
	instance.RelaylogCoordinates.LogFile = m.GetString("relay_log_file")
	instance.RelaylogCoordinates.LogPos = m.GetInt64("relay_log_pos")
	instance.RelaylogCoordinates.Type = RelayLog
	instance.LastSQLError = m.GetString("last_sql_error")
	instance.LastIOError = m.GetString("last_io_error")
	instance.SecondsBehindMain = m.GetNullInt64("seconds_behind_main")
	instance.SubordinateLagSeconds = m.GetNullInt64("subordinate_lag_seconds")
	instance.SQLDelay = m.GetUint("sql_delay")
	subordinateHostsJSON := m.GetString("subordinate_hosts")
	instance.ClusterName = m.GetString("cluster_name")
	instance.SuggestedClusterAlias = m.GetString("suggested_cluster_alias")
	instance.DataCenter = m.GetString("data_center")
	instance.PhysicalEnvironment = m.GetString("physical_environment")
	instance.SemiSyncEnforced = m.GetBool("semi_sync_enforced")
	instance.SemiSyncMainEnabled = m.GetBool("semi_sync_main_enabled")
	instance.SemiSyncReplicaEnabled = m.GetBool("semi_sync_replica_enabled")
	instance.ReplicationDepth = m.GetUint("replication_depth")
	instance.IsCoMain = m.GetBool("is_co_main")
	instance.ReplicationCredentialsAvailable = m.GetBool("replication_credentials_available")
	instance.HasReplicationCredentials = m.GetBool("has_replication_credentials")
	instance.IsUpToDate = (m.GetUint("seconds_since_last_checked") <= config.Config.InstancePollSeconds)
	instance.IsRecentlyChecked = (m.GetUint("seconds_since_last_checked") <= config.Config.InstancePollSeconds*5)
	instance.LastSeenTimestamp = m.GetString("last_seen")
	instance.IsLastCheckValid = m.GetBool("is_last_check_valid")
	instance.SecondsSinceLastSeen = m.GetNullInt64("seconds_since_last_seen")
	instance.IsCandidate = m.GetBool("is_candidate")
	instance.PromotionRule = CandidatePromotionRule(m.GetString("promotion_rule"))
	instance.IsDowntimed = m.GetBool("is_downtimed")
	instance.DowntimeReason = m.GetString("downtime_reason")
	instance.DowntimeOwner = m.GetString("downtime_owner")
	instance.DowntimeEndTimestamp = m.GetString("downtime_end_timestamp")
	instance.ElapsedDowntime = time.Second * time.Duration(m.GetInt("elapsed_downtime_seconds"))
	instance.UnresolvedHostname = m.GetString("unresolved_hostname")
	instance.AllowTLS = m.GetBool("allow_tls")
	instance.InstanceAlias = m.GetString("instance_alias")
	instance.LastDiscoveryLatency = time.Duration(m.GetInt64("last_discovery_latency")) * time.Nanosecond

	instance.SubordinateHosts.ReadJson(subordinateHostsJSON)
	instance.applyFlavorName()
	return instance
}

// readInstancesByCondition is a generic function to read instances from the backend database
func readInstancesByCondition(condition string, args []interface{}, sort string) ([](*Instance), error) {
	readFunc := func() ([](*Instance), error) {
		instances := [](*Instance){}

		if sort == "" {
			sort = `hostname, port`
		}
		query := fmt.Sprintf(`
		select
			*,
			unix_timestamp() - unix_timestamp(last_checked) as seconds_since_last_checked,
			ifnull(last_checked <= last_seen, 0) as is_last_check_valid,
			unix_timestamp() - unix_timestamp(last_seen) as seconds_since_last_seen,
			candidate_database_instance.last_suggested is not null
				 and candidate_database_instance.promotion_rule in ('must', 'prefer') as is_candidate,
			ifnull(nullif(candidate_database_instance.promotion_rule, ''), 'neutral') as promotion_rule,
			ifnull(unresolved_hostname, '') as unresolved_hostname,
			(database_instance_downtime.downtime_active is not null and ifnull(database_instance_downtime.end_timestamp, now()) > now()) as is_downtimed,
    	ifnull(database_instance_downtime.reason, '') as downtime_reason,
			ifnull(database_instance_downtime.owner, '') as downtime_owner,
			ifnull(unix_timestamp() - unix_timestamp(begin_timestamp), 0) as elapsed_downtime_seconds,
    	ifnull(database_instance_downtime.end_timestamp, '') as downtime_end_timestamp
		from
			database_instance
			left join candidate_database_instance using (hostname, port)
			left join hostname_unresolve using (hostname)
			left join database_instance_downtime using (hostname, port)
		where
			%s
		order by
			%s
			`, condition, sort)

		err := db.QueryOrchestrator(query, args, func(m sqlutils.RowMap) error {
			instance := readInstanceRow(m)
			instances = append(instances, instance)
			return nil
		})
		if err != nil {
			return instances, log.Errore(err)
		}
		err = PopulateInstancesAgents(instances)
		if err != nil {
			return instances, log.Errore(err)
		}
		return instances, err
	}
	instanceReadChan <- true
	instances, err := readFunc()
	<-instanceReadChan
	return instances, err
}

// ReadInstance reads an instance from the orchestrator backend database
func ReadInstance(instanceKey *InstanceKey) (*Instance, bool, error) {
	condition := `
			hostname = ?
			and port = ?
		`
	instances, err := readInstancesByCondition(condition, sqlutils.Args(instanceKey.Hostname, instanceKey.Port), "")
	// We know there will be at most one (hostname & port are PK)
	// And we expect to find one
	readInstanceCounter.Inc(1)
	if len(instances) == 0 {
		return nil, false, err
	}
	if err != nil {
		return instances[0], false, err
	}
	return instances[0], true, nil
}

// ReadClusterInstances reads all instances of a given cluster
func ReadClusterInstances(clusterName string) ([](*Instance), error) {
	if strings.Index(clusterName, "'") >= 0 {
		return [](*Instance){}, log.Errorf("Invalid cluster name: %s", clusterName)
	}
	condition := `cluster_name = ?`
	return readInstancesByCondition(condition, sqlutils.Args(clusterName), "")
}

// ReadClusterWriteableMain returns the/a writeable main of this cluster
// Typically, the cluster name indicates the main of the cluster. However, in circular
// main-main replication one main can assume the name of the cluster, and it is
// not guaranteed that it is the writeable one.
func ReadClusterWriteableMain(clusterName string) ([](*Instance), error) {
	condition := `
		cluster_name = ?
		and read_only = 0
		and (replication_depth = 0 or is_co_main)
	`
	return readInstancesByCondition(condition, sqlutils.Args(clusterName), "replication_depth asc")
}

// ReadWriteableClustersMains returns writeable mains of all clusters, but only one
// per cluster, in similar logic to ReadClusterWriteableMain
func ReadWriteableClustersMains() (instances [](*Instance), err error) {
	condition := `
		read_only = 0
		and (replication_depth = 0 or is_co_main)
	`
	allMains, err := readInstancesByCondition(condition, sqlutils.Args(), "cluster_name asc, replication_depth asc")
	if err != nil {
		return instances, err
	}
	visitedClusters := make(map[string]bool)
	for _, instance := range allMains {
		if !visitedClusters[instance.ClusterName] {
			visitedClusters[instance.ClusterName] = true
			instances = append(instances, instance)
		}
	}
	return instances, err
}

// ReadReplicaInstances reads replicas of a given main
func ReadReplicaInstances(mainKey *InstanceKey) ([](*Instance), error) {
	condition := `
			main_host = ?
			and main_port = ?
		`
	return readInstancesByCondition(condition, sqlutils.Args(mainKey.Hostname, mainKey.Port), "")
}

// ReadReplicaInstancesIncludingBinlogServerSubReplicas returns a list of direct slves including any replicas
// of a binlog server replica
func ReadReplicaInstancesIncludingBinlogServerSubReplicas(mainKey *InstanceKey) ([](*Instance), error) {
	replicas, err := ReadReplicaInstances(mainKey)
	if err != nil {
		return replicas, err
	}
	for _, replica := range replicas {
		replica := replica
		if replica.IsBinlogServer() {
			binlogServerReplicas, err := ReadReplicaInstancesIncludingBinlogServerSubReplicas(&replica.Key)
			if err != nil {
				return replicas, err
			}
			replicas = append(replicas, binlogServerReplicas...)
		}
	}
	return replicas, err
}

// ReadBinlogServerReplicaInstances reads direct replicas of a given main that are binlog servers
func ReadBinlogServerReplicaInstances(mainKey *InstanceKey) ([](*Instance), error) {
	condition := `
			main_host = ?
			and main_port = ?
			and binlog_server = 1
		`
	return readInstancesByCondition(condition, sqlutils.Args(mainKey.Hostname, mainKey.Port), "")
}

// ReadUnseenInstances reads all instances which were not recently seen
func ReadUnseenInstances() ([](*Instance), error) {
	condition := `last_seen < last_checked`
	return readInstancesByCondition(condition, sqlutils.Args(), "")
}

// ReadProblemInstances reads all instances with problems
func ReadProblemInstances(clusterName string) ([](*Instance), error) {
	condition := `
			cluster_name LIKE (CASE WHEN ? = '' THEN '%' ELSE ? END)
			and (
				(last_seen < last_checked)
				or (unix_timestamp() - unix_timestamp(last_checked) > ?)
				or (not subordinate_sql_running)
				or (not subordinate_io_running)
				or (abs(cast(seconds_behind_main as signed) - cast(sql_delay as signed)) > ?)
				or (abs(cast(subordinate_lag_seconds as signed) - cast(sql_delay as signed)) > ?)
			)
		`

	args := sqlutils.Args(clusterName, clusterName, config.Config.InstancePollSeconds, config.Config.ReasonableReplicationLagSeconds, config.Config.ReasonableReplicationLagSeconds)
	instances, err := readInstancesByCondition(condition, args, "")
	if err != nil {
		return instances, err
	}
	var reportedInstances [](*Instance)
	for _, instance := range instances {
		skip := false
		if instance.IsDowntimed {
			skip = true
		}
		if RegexpMatchPatterns(instance.Key.Hostname, config.Config.ProblemIgnoreHostnameFilters) {
			skip = true
		}
		if !skip {
			reportedInstances = append(reportedInstances, instance)
		}
	}
	return reportedInstances, nil
}

// SearchInstances reads all instances qualifying for some searchString
func SearchInstances(searchString string) ([](*Instance), error) {
	searchString = strings.TrimSpace(searchString)
	condition := `
			instr(hostname, ?) > 0
			or instr(cluster_name, ?) > 0
			or instr(version, ?) > 0
			or instr(version_comment, ?) > 0
			or instr(concat(hostname, ':', port), ?) > 0
			or concat(server_id, '') = ?
			or concat(port, '') = ?
		`
	args := sqlutils.Args(searchString, searchString, searchString, searchString, searchString, searchString, searchString)
	return readInstancesByCondition(condition, args, `replication_depth asc, num_subordinate_hosts desc, cluster_name, hostname, port`)
}

// FindInstances reads all instances whose name matches given pattern
func FindInstances(regexpPattern string) (result [](*Instance), err error) {
	result = [](*Instance){}
	r, err := regexp.Compile(regexpPattern)
	if err != nil {
		return result, err
	}
	condition := `1=1`
	unfiltered, err := readInstancesByCondition(condition, sqlutils.Args(), `replication_depth asc, num_subordinate_hosts desc, cluster_name, hostname, port`)
	if err != nil {
		return unfiltered, err
	}
	for _, instance := range unfiltered {
		if r.MatchString(instance.Key.DisplayString()) {
			result = append(result, instance)
		}
	}
	return result, nil
}

// FindFuzzyInstances return instances whose names are like the one given (host & port substrings)
// For example, the given `mydb-3:3306` might find `myhosts-mydb301-production.mycompany.com:3306`
func FindFuzzyInstances(fuzzyInstanceKey *InstanceKey) ([](*Instance), error) {
	condition := `
		hostname like concat('%%', ?, '%%')
		and port = ?
	`
	return readInstancesByCondition(condition, sqlutils.Args(fuzzyInstanceKey.Hostname, fuzzyInstanceKey.Port), `replication_depth asc, num_subordinate_hosts desc, cluster_name, hostname, port`)
}

// ReadFuzzyInstanceKey accepts a fuzzy instance key and expects to return a single, fully qualified,
// known instance key.
func ReadFuzzyInstanceKey(fuzzyInstanceKey *InstanceKey) *InstanceKey {
	if fuzzyInstanceKey == nil {
		return nil
	}
	if fuzzyInstanceKey.Hostname != "" {
		// Fuzzy instance search
		if fuzzyInstances, _ := FindFuzzyInstances(fuzzyInstanceKey); len(fuzzyInstances) == 1 {
			return &(fuzzyInstances[0].Key)
		}
	}
	return nil
}

// ReadFuzzyInstanceKeyIfPossible accepts a fuzzy instance key and hopes to return a single, fully qualified,
// known instance key, or else the original given key
func ReadFuzzyInstanceKeyIfPossible(fuzzyInstanceKey *InstanceKey) *InstanceKey {
	if instanceKey := ReadFuzzyInstanceKey(fuzzyInstanceKey); instanceKey != nil {
		return instanceKey
	}
	return fuzzyInstanceKey
}

// ReadFuzzyInstance accepts a fuzzy instance key and expects to return a single instance.
// Multiple instances matching the fuzzy keys are not allowed.
func ReadFuzzyInstance(fuzzyInstanceKey *InstanceKey) (*Instance, error) {
	if fuzzyInstanceKey == nil {
		return nil, log.Errorf("ReadFuzzyInstance received nil input")
	}
	if fuzzyInstanceKey.Hostname != "" {
		// Fuzzy instance search
		if fuzzyInstances, _ := FindFuzzyInstances(fuzzyInstanceKey); len(fuzzyInstances) == 1 {
			return fuzzyInstances[0], nil
		}
	}
	return nil, log.Errorf("Cannot determine fuzzy instance %+v", *fuzzyInstanceKey)
}

// ReadLostInRecoveryInstances returns all instances (potentially filtered by cluster)
// which are currently indicated as downtimed due to being lost during a topology recovery.
func ReadLostInRecoveryInstances(clusterName string) ([](*Instance), error) {
	condition := `
		ifnull(
			database_instance_downtime.downtime_active = 1
			and database_instance_downtime.end_timestamp > now()
			and database_instance_downtime.reason = ?, 0)
		and ? IN ('', cluster_name)
	`
	return readInstancesByCondition(condition, sqlutils.Args(DowntimeLostInRecoveryMessage, clusterName), "cluster_name asc, replication_depth asc")
}

// ReadDowntimedInstances returns all instances currently downtimed, potentially filtered by cluster
func ReadDowntimedInstances(clusterName string) ([](*Instance), error) {
	condition := `
		ifnull(
			database_instance_downtime.downtime_active = 1
			and database_instance_downtime.end_timestamp > now()
			, 0)
		and ? IN ('', cluster_name)
	`
	return readInstancesByCondition(condition, sqlutils.Args(clusterName), "cluster_name asc, replication_depth asc")
}

// ReadClusterCandidateInstances reads cluster instances which are also marked as candidates
func ReadClusterCandidateInstances(clusterName string) ([](*Instance), error) {
	condition := `
			cluster_name = ?
			and concat(hostname, ':', port) in (
				select concat(hostname, ':', port)
					from candidate_database_instance
					where promotion_rule in ('must', 'prefer')
			)
			`
	return readInstancesByCondition(condition, sqlutils.Args(clusterName), "")
}

// ReadClusterNeutralPromotionRuleInstances reads cluster instances whose promotion-rule is marked as 'neutral'
func ReadClusterNeutralPromotionRuleInstances(clusterName string) (neutralInstances [](*Instance), err error) {
	instances, err := ReadClusterInstances(clusterName)
	if err != nil {
		return neutralInstances, err
	}
	for _, instance := range instances {
		if instance.PromotionRule == NeutralPromoteRule {
			neutralInstances = append(neutralInstances, instance)
		}
	}
	return neutralInstances, nil
}

// filterOSCInstances will filter the given list such that only replicas fit for OSC control remain.
func filterOSCInstances(instances [](*Instance)) [](*Instance) {
	result := [](*Instance){}
	for _, instance := range instances {
		if RegexpMatchPatterns(instance.Key.Hostname, config.Config.OSCIgnoreHostnameFilters) {
			continue
		}
		if instance.IsBinlogServer() {
			continue
		}
		if !instance.IsLastCheckValid {
			continue
		}
		result = append(result, instance)
	}
	return result
}

// GetClusterOSCReplicas returns a heuristic list of replicas which are fit as controll replicas for an OSC operation.
// These would be intermediate mains
func GetClusterOSCReplicas(clusterName string) ([](*Instance), error) {
	intermediateMains := [](*Instance){}
	result := [](*Instance){}
	var err error
	if strings.Index(clusterName, "'") >= 0 {
		return [](*Instance){}, log.Errorf("Invalid cluster name: %s", clusterName)
	}
	{
		// Pick up to two busiest IMs
		condition := `
			replication_depth = 1
			and num_subordinate_hosts > 0
			and cluster_name = ?
		`
		intermediateMains, err = readInstancesByCondition(condition, sqlutils.Args(clusterName), "")
		if err != nil {
			return result, err
		}
		sort.Sort(sort.Reverse(InstancesByCountSubordinateHosts(intermediateMains)))
		intermediateMains = filterOSCInstances(intermediateMains)
		intermediateMains = intermediateMains[0:math.MinInt(2, len(intermediateMains))]
		result = append(result, intermediateMains...)
	}
	{
		// Get 2 replicas of found IMs, if possible
		if len(intermediateMains) == 1 {
			// Pick 2 replicas for this IM
			replicas, err := ReadReplicaInstances(&(intermediateMains[0].Key))
			if err != nil {
				return result, err
			}
			sort.Sort(sort.Reverse(InstancesByCountSubordinateHosts(replicas)))
			replicas = filterOSCInstances(replicas)
			replicas = replicas[0:math.MinInt(2, len(replicas))]
			result = append(result, replicas...)

		}
		if len(intermediateMains) == 2 {
			// Pick one replica from each IM (should be possible)
			for _, im := range intermediateMains {
				replicas, err := ReadReplicaInstances(&im.Key)
				if err != nil {
					return result, err
				}
				sort.Sort(sort.Reverse(InstancesByCountSubordinateHosts(replicas)))
				replicas = filterOSCInstances(replicas)
				if len(replicas) > 0 {
					result = append(result, replicas[0])
				}
			}
		}
	}
	{
		// Get 2 3rd tier replicas, if possible
		condition := `
			replication_depth = 3
			and cluster_name = ?
		`
		replicas, err := readInstancesByCondition(condition, sqlutils.Args(clusterName), "")
		if err != nil {
			return result, err
		}
		sort.Sort(sort.Reverse(InstancesByCountSubordinateHosts(replicas)))
		replicas = filterOSCInstances(replicas)
		replicas = replicas[0:math.MinInt(2, len(replicas))]
		result = append(result, replicas...)
	}
	{
		// Get 2 1st tier leaf replicas, if possible
		condition := `
			replication_depth = 1
			and num_subordinate_hosts = 0
			and cluster_name = ?
		`
		replicas, err := readInstancesByCondition(condition, sqlutils.Args(clusterName), "")
		if err != nil {
			return result, err
		}
		replicas = filterOSCInstances(replicas)
		replicas = replicas[0:math.MinInt(2, len(replicas))]
		result = append(result, replicas...)
	}

	return result, nil
}

// GetClusterGhostReplicas returns a list of replicas that can serve as the connected servers
// for a [gh-ost](https://github.com/github/gh-ost) operation. A gh-ost operation prefers to talk
// to a RBR replica that has no children.
func GetClusterGhostReplicas(clusterName string) (result [](*Instance), err error) {
	condition := `
			replication_depth > 0
			and binlog_format = 'ROW'
			and cluster_name = ?
		`
	instances, err := readInstancesByCondition(condition, sqlutils.Args(clusterName), "num_subordinate_hosts asc")
	if err != nil {
		return result, err
	}

	for _, instance := range instances {
		skipThisHost := false
		if instance.IsBinlogServer() {
			skipThisHost = true
		}
		if !instance.IsLastCheckValid {
			skipThisHost = true
		}
		if !instance.LogBinEnabled {
			skipThisHost = true
		}
		if !instance.LogSubordinateUpdatesEnabled {
			skipThisHost = true
		}
		if !skipThisHost {
			result = append(result, instance)
		}
	}

	return result, err
}

// GetInstancesMaxLag returns the maximum lag in a set of instances
func GetInstancesMaxLag(instances [](*Instance)) (maxLag int64, err error) {
	if len(instances) == 0 {
		return 0, log.Errorf("No instances found in GetInstancesMaxLag")
	}
	for _, clusterInstance := range instances {
		if clusterInstance.SubordinateLagSeconds.Valid && clusterInstance.SubordinateLagSeconds.Int64 > maxLag {
			maxLag = clusterInstance.SubordinateLagSeconds.Int64
		}
	}
	return maxLag, nil
}

// GetClusterHeuristicLag returns a heuristic lag for a cluster, based on its OSC replicas
func GetClusterHeuristicLag(clusterName string) (int64, error) {
	instances, err := GetClusterOSCReplicas(clusterName)
	if err != nil {
		return 0, err
	}
	return GetInstancesMaxLag(instances)
}

// GetHeuristicClusterPoolInstances returns instances of a cluster which are also pooled. If `pool` argument
// is empty, all pools are considered, otherwise, only instances of given pool are considered.
func GetHeuristicClusterPoolInstances(clusterName string, pool string) (result [](*Instance), err error) {
	result = [](*Instance){}
	instances, err := ReadClusterInstances(clusterName)
	if err != nil {
		return result, err
	}

	pooledInstanceKeys := NewInstanceKeyMap()
	clusterPoolInstances, err := ReadClusterPoolInstances(clusterName, pool)
	if err != nil {
		return result, err
	}
	for _, clusterPoolInstance := range clusterPoolInstances {
		pooledInstanceKeys.AddKey(InstanceKey{Hostname: clusterPoolInstance.Hostname, Port: clusterPoolInstance.Port})
	}

	for _, instance := range instances {
		skipThisHost := false
		if instance.IsBinlogServer() {
			skipThisHost = true
		}
		if !instance.IsLastCheckValid {
			skipThisHost = true
		}
		if !pooledInstanceKeys.HasKey(instance.Key) {
			skipThisHost = true
		}
		if !skipThisHost {
			result = append(result, instance)
		}
	}

	return result, err
}

// GetHeuristicClusterPoolInstancesLag returns a heuristic lag for the instances participating
// in a cluster pool (or all the cluster's pools)
func GetHeuristicClusterPoolInstancesLag(clusterName string, pool string) (int64, error) {
	instances, err := GetHeuristicClusterPoolInstances(clusterName, pool)
	if err != nil {
		return 0, err
	}
	return GetInstancesMaxLag(instances)
}

// updateInstanceClusterName
func updateInstanceClusterName(instance *Instance) error {
	writeFunc := func() error {
		_, err := db.ExecOrchestrator(`
			update
				database_instance
			set
				cluster_name=?
			where
				hostname=? and port=?
        	`, instance.ClusterName, instance.Key.Hostname, instance.Key.Port,
		)
		if err != nil {
			return log.Errore(err)
		}
		AuditOperation("update-cluster-name", &instance.Key, fmt.Sprintf("set to %s", instance.ClusterName))
		return nil
	}
	return ExecDBWriteFunc(writeFunc)
}

// ReviewUnseenInstances reviews instances that have not been seen (suposedly dead) and updates some of their data
func ReviewUnseenInstances() error {
	instances, err := ReadUnseenInstances()
	if err != nil {
		return log.Errore(err)
	}
	operations := 0
	for _, instance := range instances {
		instance := instance

		mainHostname, err := ResolveHostname(instance.MainKey.Hostname)
		if err != nil {
			log.Errore(err)
			continue
		}
		instance.MainKey.Hostname = mainHostname
		savedClusterName := instance.ClusterName

		if err := ReadInstanceClusterAttributes(instance); err != nil {
			log.Errore(err)
		} else if instance.ClusterName != savedClusterName {
			updateInstanceClusterName(instance)
			operations++
		}
	}

	AuditOperation("review-unseen-instances", nil, fmt.Sprintf("Operations: %d", operations))
	return err
}

// readUnseenMainKeys will read list of mains that have never been seen, and yet whose replicas
// seem to be replicating.
func readUnseenMainKeys() ([]InstanceKey, error) {
	res := []InstanceKey{}

	err := db.QueryOrchestratorRowsMap(`
			SELECT DISTINCT
			    subordinate_instance.main_host, subordinate_instance.main_port
			FROM
			    database_instance subordinate_instance
			        LEFT JOIN
			    hostname_resolve ON (subordinate_instance.main_host = hostname_resolve.hostname)
			        LEFT JOIN
			    database_instance main_instance ON (
			    	COALESCE(hostname_resolve.resolved_hostname, subordinate_instance.main_host) = main_instance.hostname
			    	and subordinate_instance.main_port = main_instance.port)
			WHERE
			    main_instance.last_checked IS NULL
			    and subordinate_instance.main_host != ''
			    and subordinate_instance.main_host != '_'
			    and subordinate_instance.main_port > 0
			    and subordinate_instance.subordinate_io_running = 1
			`, func(m sqlutils.RowMap) error {
		instanceKey, _ := NewInstanceKeyFromStrings(m.GetString("main_host"), m.GetString("main_port"))
		// we ignore the error. It can be expected that we are unable to resolve the hostname.
		// Maybe that's how we got here in the first place!
		res = append(res, *instanceKey)

		return nil
	})
	if err != nil {
		return res, log.Errore(err)
	}

	return res, nil
}

// InjectUnseenMains will review mains of instances that are known to be replicating, yet which are not listed
// in database_instance. Since their replicas are listed as replicating, we can assume that such mains actually do
// exist: we shall therefore inject them with minimal details into the database_instance table.
func InjectUnseenMains() error {

	unseenMainKeys, err := readUnseenMainKeys()
	if err != nil {
		return err
	}

	operations := 0
	for _, mainKey := range unseenMainKeys {
		mainKey := mainKey
		clusterName := mainKey.StringCode()
		// minimal details:
		instance := Instance{Key: mainKey, Version: "Unknown", ClusterName: clusterName}
		if err := WriteInstance(&instance, false, nil); err == nil {
			operations++
		}
	}

	AuditOperation("inject-unseen-mains", nil, fmt.Sprintf("Operations: %d", operations))
	return err
}

// ForgetUnseenInstancesDifferentlyResolved will purge instances which are invalid, and whose hostname
// appears on the hostname_resolved table; this means some time in the past their hostname was unresovled, and now
// resovled to a different value; the old hostname is never accessed anymore and the old entry should be removed.
func ForgetUnseenInstancesDifferentlyResolved() error {
	query := `
			select
				database_instance.hostname, database_instance.port
			from
					hostname_resolve
					JOIN database_instance ON (hostname_resolve.hostname = database_instance.hostname)
			where
					hostname_resolve.hostname != hostname_resolve.resolved_hostname
					AND ifnull(last_checked <= last_seen, 0) = 0
	`
	keys := NewInstanceKeyMap()
	err := db.QueryOrchestrator(query, nil, func(m sqlutils.RowMap) error {
		key := InstanceKey{
			Hostname: m.GetString("hostname"),
			Port:     m.GetInt("port"),
		}
		keys.AddKey(key)
		return nil
	})
	var rowsAffected int64 = 0
	for _, key := range keys.GetInstanceKeys() {
		sqlResult, err := db.ExecOrchestrator(`
			delete from
				database_instance
			where
		    hostname = ? and port = ?
			`, sqlutils.Args(key.Hostname, key.Port),
		)
		if err != nil {
			return log.Errore(err)
		}
		rows, err := sqlResult.RowsAffected()
		if err != nil {
			return log.Errore(err)
		}
		rowsAffected = rowsAffected + rows
	}
	AuditOperation("forget-unseen-differently-resolved", nil, fmt.Sprintf("Forgotten instances: %d", rowsAffected))
	return err
}

// readUnknownMainHostnameResolves will figure out the resolved hostnames of main-hosts which cannot be found.
// It uses the hostname_resolve_history table to heuristically guess the correct hostname (based on "this was the
// last time we saw this hostname and it resolves into THAT")
func readUnknownMainHostnameResolves() (map[string]string, error) {
	res := make(map[string]string)
	err := db.QueryOrchestratorRowsMap(`
			SELECT DISTINCT
			    subordinate_instance.main_host, hostname_resolve_history.resolved_hostname
			FROM
			    database_instance subordinate_instance
			LEFT JOIN hostname_resolve ON (subordinate_instance.main_host = hostname_resolve.hostname)
			LEFT JOIN database_instance main_instance ON (
			    COALESCE(hostname_resolve.resolved_hostname, subordinate_instance.main_host) = main_instance.hostname
			    and subordinate_instance.main_port = main_instance.port
			) LEFT JOIN hostname_resolve_history ON (subordinate_instance.main_host = hostname_resolve_history.hostname)
			WHERE
			    main_instance.last_checked IS NULL
			    and subordinate_instance.main_host != ''
			    and subordinate_instance.main_host != '_'
			    and subordinate_instance.main_port > 0
			`, func(m sqlutils.RowMap) error {
		res[m.GetString("main_host")] = m.GetString("resolved_hostname")
		return nil
	})
	if err != nil {
		return res, log.Errore(err)
	}

	return res, nil
}

// ResolveUnknownMainHostnameResolves fixes missing hostname resolves based on hostname_resolve_history
// The use case is replicas replicating from some unknown-hostname which cannot be otherwise found. This could
// happen due to an expire unresolve together with clearing up of hostname cache.
func ResolveUnknownMainHostnameResolves() error {

	hostnameResolves, err := readUnknownMainHostnameResolves()
	if err != nil {
		return err
	}
	for hostname, resolvedHostname := range hostnameResolves {
		UpdateResolvedHostname(hostname, resolvedHostname)
	}

	AuditOperation("resolve-unknown-mains", nil, fmt.Sprintf("Num resolved hostnames: %d", len(hostnameResolves)))
	return err
}

// ReadCountMySQLSnapshots is a utility method to return registered number of snapshots for a given list of hosts
func ReadCountMySQLSnapshots(hostnames []string) (map[string]int, error) {
	res := make(map[string]int)
	if !config.Config.ServeAgentsHttp {
		return res, nil
	}
	query := fmt.Sprintf(`
		select
			hostname,
			count_mysql_snapshots
		from
			host_agent
		where
			hostname in (%s)
		order by
			hostname
		`, sqlutils.InClauseStringValues(hostnames))

	err := db.QueryOrchestratorRowsMap(query, func(m sqlutils.RowMap) error {
		res[m.GetString("hostname")] = m.GetInt("count_mysql_snapshots")
		return nil
	})

	if err != nil {
		log.Errore(err)
	}
	return res, err
}

// PopulateInstancesAgents will fill in extra data acquired from agents for given instances
// At current this is the number of snapshots.
// This isn't too pretty; it's a push-into-instance-data-that-belongs-to-agent thing.
// Originally the need was to visually present the number of snapshots per host on the web/cluster page, which
// indeed proves to be useful in our experience.
func PopulateInstancesAgents(instances [](*Instance)) error {
	if len(instances) == 0 {
		return nil
	}
	hostnames := []string{}
	for _, instance := range instances {
		hostnames = append(hostnames, instance.Key.Hostname)
	}
	agentsCountMySQLSnapshots, err := ReadCountMySQLSnapshots(hostnames)
	if err != nil {
		return err
	}
	for _, instance := range instances {
		if count, ok := agentsCountMySQLSnapshots[instance.Key.Hostname]; ok {
			instance.CountMySQLSnapshots = count
		}
	}

	return nil
}

func GetClusterName(instanceKey *InstanceKey) (clusterName string, err error) {
	if clusterName, found := instanceKeyInformativeClusterName.Get(instanceKey.StringCode()); found {
		return clusterName.(string), nil
	}
	query := `
		select
			ifnull(max(cluster_name), '') as cluster_name
		from
			database_instance
		where
			hostname = ?
			and port = ?
			`
	err = db.QueryOrchestrator(query, sqlutils.Args(instanceKey.Hostname, instanceKey.Port), func(m sqlutils.RowMap) error {
		clusterName = m.GetString("cluster_name")
		instanceKeyInformativeClusterName.Set(instanceKey.StringCode(), clusterName, cache.DefaultExpiration)
		return nil
	})

	return clusterName, log.Errore(err)
}

// ReadClusters reads names of all known clusters
func ReadClusters() (clusterNames []string, err error) {
	clusters, err := ReadClustersInfo("")
	if err != nil {
		return clusterNames, err
	}
	for _, clusterInfo := range clusters {
		clusterNames = append(clusterNames, clusterInfo.ClusterName)
	}
	return clusterNames, nil
}

// ReadClusterInfo reads some info about a given cluster
func ReadClusterInfo(clusterName string) (*ClusterInfo, error) {
	clusters, err := ReadClustersInfo(clusterName)
	if err != nil {
		return &ClusterInfo{}, err
	}
	if len(clusters) != 1 {
		return &ClusterInfo{}, fmt.Errorf("No cluster info found for %s", clusterName)
	}
	return &(clusters[0]), nil
}

// ReadClustersInfo reads names of all known clusters and some aggregated info
func ReadClustersInfo(clusterName string) ([]ClusterInfo, error) {
	clusters := []ClusterInfo{}

	whereClause := ""
	args := sqlutils.Args()
	if clusterName != "" {
		whereClause = `where cluster_name = ?`
		args = append(args, clusterName)
	}
	query := fmt.Sprintf(`
		select
			cluster_name,
			count(*) as count_instances,
			ifnull(min(alias), cluster_name) as alias,
			ifnull(min(domain_name), '') as domain_name
		from
			database_instance
			left join cluster_alias using (cluster_name)
			left join cluster_domain_name using (cluster_name)
		%s
		group by
			cluster_name`, whereClause)

	err := db.QueryOrchestrator(query, args, func(m sqlutils.RowMap) error {
		clusterInfo := ClusterInfo{
			ClusterName:    m.GetString("cluster_name"),
			CountInstances: m.GetUint("count_instances"),
			ClusterAlias:   m.GetString("alias"),
			ClusterDomain:  m.GetString("domain_name"),
		}
		clusterInfo.ApplyClusterAlias()
		clusterInfo.ReadRecoveryInfo()

		clusters = append(clusters, clusterInfo)
		return nil
	})

	return clusters, err
}

// Get a listing of KVPair for clusters mains, for all clusters or for a specific cluster.
func GetMainsKVPairs(clusterName string) (kvPairs [](*kv.KVPair), err error) {

	clusterAliasMap := make(map[string]string)
	if clustersInfo, err := ReadClustersInfo(clusterName); err != nil {
		return kvPairs, err
	} else {
		for _, clusterInfo := range clustersInfo {
			clusterAliasMap[clusterInfo.ClusterName] = clusterInfo.ClusterAlias
		}
	}

	mains, err := ReadWriteableClustersMains()
	if err != nil {
		return kvPairs, err
	}
	for _, main := range mains {
		clusterPairs := GetClusterMainKVPairs(clusterAliasMap[main.ClusterName], &main.Key)
		kvPairs = append(kvPairs, clusterPairs...)
	}

	return kvPairs, err
}

// HeuristicallyApplyClusterDomainInstanceAttribute writes down the cluster-domain
// to main-hostname as a general attribute, by reading current topology and **trusting** it to be correct
func HeuristicallyApplyClusterDomainInstanceAttribute(clusterName string) (instanceKey *InstanceKey, err error) {
	clusterInfo, err := ReadClusterInfo(clusterName)
	if err != nil {
		return nil, err
	}

	if clusterInfo.ClusterDomain == "" {
		return nil, fmt.Errorf("Cannot find domain name for cluster %+v", clusterName)
	}

	mains, err := ReadClusterWriteableMain(clusterName)
	if err != nil {
		return nil, err
	}
	if len(mains) != 1 {
		return nil, fmt.Errorf("Found %+v potential main for cluster %+v", len(mains), clusterName)
	}
	instanceKey = &mains[0].Key
	return instanceKey, attributes.SetGeneralAttribute(clusterInfo.ClusterDomain, instanceKey.StringCode())
}

// GetHeuristicClusterDomainInstanceAttribute attempts detecting the cluster domain
// for the given cluster, and return the instance key associated as writer with that domain
func GetHeuristicClusterDomainInstanceAttribute(clusterName string) (instanceKey *InstanceKey, err error) {
	clusterInfo, err := ReadClusterInfo(clusterName)
	if err != nil {
		return nil, err
	}

	if clusterInfo.ClusterDomain == "" {
		return nil, fmt.Errorf("Cannot find domain name for cluster %+v", clusterName)
	}

	writerInstanceName, err := attributes.GetGeneralAttribute(clusterInfo.ClusterDomain)
	if err != nil {
		return nil, err
	}
	return NewRawInstanceKey(writerInstanceName)
}

// ReadAllInstanceKeys
func ReadAllInstanceKeys() ([]InstanceKey, error) {
	res := []InstanceKey{}
	query := `
		select
			hostname, port
		from
			database_instance
			`
	err := db.QueryOrchestrator(query, sqlutils.Args(), func(m sqlutils.RowMap) error {
		instanceKey, merr := NewInstanceKeyFromStrings(m.GetString("hostname"), m.GetString("port"))
		if merr != nil {
			log.Errore(merr)
		} else if !InstanceIsForgotten(instanceKey) {
			// only if not in "forget" cache
			res = append(res, *instanceKey)
		}
		return nil
	})
	return res, log.Errore(err)
}

// ReadAllInstanceKeysMainKeys
func ReadAllMinimalInstances() ([]MinimalInstance, error) {
	res := []MinimalInstance{}
	query := `
		select
			hostname, port, main_host, main_port, cluster_name
		from
			database_instance
			`
	err := db.QueryOrchestrator(query, sqlutils.Args(), func(m sqlutils.RowMap) error {
		minimalInstance := MinimalInstance{}
		minimalInstance.Key = InstanceKey{
			Hostname: m.GetString("hostname"),
			Port:     m.GetInt("port"),
		}
		minimalInstance.MainKey = InstanceKey{
			Hostname: m.GetString("main_host"),
			Port:     m.GetInt("main_port"),
		}
		minimalInstance.ClusterName = m.GetString("cluster_name")

		if !InstanceIsForgotten(&minimalInstance.Key) {
			// only if not in "forget" cache
			res = append(res, minimalInstance)
		}
		return nil
	})
	return res, log.Errore(err)
}

// ReadOutdatedInstanceKeys reads and returns keys for all instances that are not up to date (i.e.
// pre-configured time has passed since they were last checked)
// But we also check for the case where an attempt at instance checking has been made, that hasn't
// resulted in an actual check! This can happen when TCP/IP connections are hung, in which case the "check"
// never returns. In such case we multiply interval by a factor, so as not to open too many connections on
// the instance.
func ReadOutdatedInstanceKeys() ([]InstanceKey, error) {
	res := []InstanceKey{}
	query := `
		select
			hostname, port
		from
			database_instance
		where
			case
				when last_attempted_check <= last_checked
				then last_checked < now() - interval ? second
				else last_checked < now() - interval ? second
			end
			`
	args := sqlutils.Args(config.Config.InstancePollSeconds, 2*config.Config.InstancePollSeconds)

	err := db.QueryOrchestrator(query, args, func(m sqlutils.RowMap) error {
		instanceKey, merr := NewInstanceKeyFromStrings(m.GetString("hostname"), m.GetString("port"))
		if merr != nil {
			log.Errore(merr)
		} else if !InstanceIsForgotten(instanceKey) {
			// only if not in "forget" cache
			res = append(res, *instanceKey)
		}
		// We don;t return an error because we want to keep filling the outdated instances list.
		return nil
	})

	if err != nil {
		log.Errore(err)
	}
	return res, err

}

func mkInsertOdku(table string, columns []string, values []string, nrRows int, insertIgnore bool) (string, error) {
	if len(columns) == 0 {
		return "", errors.New("Column list cannot be empty")
	}
	if nrRows < 1 {
		return "", errors.New("nrRows must be a positive number")
	}
	if len(columns) != len(values) {
		return "", errors.New("number of values must be equal to number of columns")
	}

	var q bytes.Buffer
	var ignore string = ""
	if insertIgnore {
		ignore = "ignore"
	}
	var valRow string = fmt.Sprintf("(%s)", strings.Join(values, ", "))
	var val bytes.Buffer
	val.WriteString(valRow)
	for i := 1; i < nrRows; i++ {
		val.WriteString(",\n                ") // indent VALUES, see below
		val.WriteString(valRow)
	}

	var col string = strings.Join(columns, ", ")
	var odku bytes.Buffer
	odku.WriteString(fmt.Sprintf("%s=VALUES(%s)", columns[0], columns[0]))
	for _, c := range columns[1:] {
		odku.WriteString(", ")
		odku.WriteString(fmt.Sprintf("%s=VALUES(%s)", c, c))
	}

	q.WriteString(fmt.Sprintf(`INSERT %s INTO %s
                (%s)
        VALUES
                %s
        ON DUPLICATE KEY UPDATE
                %s
        `,
		ignore, table, col, val.String(), odku.String()))

	return q.String(), nil
}

func mkInsertOdkuForInstances(instances []*Instance, instanceWasActuallyFound bool, updateLastSeen bool) (string, []interface{}, error) {
	if len(instances) == 0 {
		return "", nil, nil
	}

	insertIgnore := false
	if !instanceWasActuallyFound {
		insertIgnore = true
	}
	var columns = []string{
		"hostname",
		"port",
		"last_checked",
		"last_attempted_check",
		"last_check_partial_success",
		"uptime",
		"server_id",
		"server_uuid",
		"version",
		"major_version",
		"version_comment",
		"binlog_server",
		"read_only",
		"binlog_format",
		"binlog_row_image",
		"log_bin",
		"log_subordinate_updates",
		"binary_log_file",
		"binary_log_pos",
		"main_host",
		"main_port",
		"subordinate_sql_running",
		"subordinate_io_running",
		"has_replication_filters",
		"supports_oracle_gtid",
		"oracle_gtid",
		"executed_gtid_set",
		"gtid_mode",
		"gtid_purged",
		"mariadb_gtid",
		"pseudo_gtid",
		"main_log_file",
		"read_main_log_pos",
		"relay_main_log_file",
		"exec_main_log_pos",
		"relay_log_file",
		"relay_log_pos",
		"last_sql_error",
		"last_io_error",
		"seconds_behind_main",
		"subordinate_lag_seconds",
		"sql_delay",
		"num_subordinate_hosts",
		"subordinate_hosts",
		"cluster_name",
		"suggested_cluster_alias",
		"data_center",
		"physical_environment",
		"replication_depth",
		"is_co_main",
		"replication_credentials_available",
		"has_replication_credentials",
		"allow_tls",
		"semi_sync_enforced",
		"semi_sync_main_enabled",
		"semi_sync_replica_enabled",
		"instance_alias",
		"last_discovery_latency",
	}

	var values []string = make([]string, len(columns), len(columns))
	for i := range columns {
		values[i] = "?"
	}
	values[2] = "NOW()" // last_checked
	values[3] = "NOW()" // last_attempted_check
	values[4] = "1"     // last_check_partial_success

	if updateLastSeen {
		columns = append(columns, "last_seen")
		values = append(values, "NOW()")
	}

	var args []interface{}
	for _, instance := range instances {
		// number of columns minus 2 as last_checked and last_attempted_check
		// updated with NOW()
		args = append(args, instance.Key.Hostname)
		args = append(args, instance.Key.Port)
		args = append(args, instance.Uptime)
		args = append(args, instance.ServerID)
		args = append(args, instance.ServerUUID)
		args = append(args, instance.Version)
		args = append(args, instance.MajorVersionString())
		args = append(args, instance.VersionComment)
		args = append(args, instance.IsBinlogServer())
		args = append(args, instance.ReadOnly)
		args = append(args, instance.Binlog_format)
		args = append(args, instance.BinlogRowImage)
		args = append(args, instance.LogBinEnabled)
		args = append(args, instance.LogSubordinateUpdatesEnabled)
		args = append(args, instance.SelfBinlogCoordinates.LogFile)
		args = append(args, instance.SelfBinlogCoordinates.LogPos)
		args = append(args, instance.MainKey.Hostname)
		args = append(args, instance.MainKey.Port)
		args = append(args, instance.Subordinate_SQL_Running)
		args = append(args, instance.Subordinate_IO_Running)
		args = append(args, instance.HasReplicationFilters)
		args = append(args, instance.SupportsOracleGTID)
		args = append(args, instance.UsingOracleGTID)
		args = append(args, instance.ExecutedGtidSet)
		args = append(args, instance.GTIDMode)
		args = append(args, instance.GtidPurged)
		args = append(args, instance.UsingMariaDBGTID)
		args = append(args, instance.UsingPseudoGTID)
		args = append(args, instance.ReadBinlogCoordinates.LogFile)
		args = append(args, instance.ReadBinlogCoordinates.LogPos)
		args = append(args, instance.ExecBinlogCoordinates.LogFile)
		args = append(args, instance.ExecBinlogCoordinates.LogPos)
		args = append(args, instance.RelaylogCoordinates.LogFile)
		args = append(args, instance.RelaylogCoordinates.LogPos)
		args = append(args, instance.LastSQLError)
		args = append(args, instance.LastIOError)
		args = append(args, instance.SecondsBehindMain)
		args = append(args, instance.SubordinateLagSeconds)
		args = append(args, instance.SQLDelay)
		args = append(args, len(instance.SubordinateHosts))
		args = append(args, instance.SubordinateHosts.ToJSONString())
		args = append(args, instance.ClusterName)
		args = append(args, instance.SuggestedClusterAlias)
		args = append(args, instance.DataCenter)
		args = append(args, instance.PhysicalEnvironment)
		args = append(args, instance.ReplicationDepth)
		args = append(args, instance.IsCoMain)
		args = append(args, instance.ReplicationCredentialsAvailable)
		args = append(args, instance.HasReplicationCredentials)
		args = append(args, instance.AllowTLS)
		args = append(args, instance.SemiSyncEnforced)
		args = append(args, instance.SemiSyncMainEnabled)
		args = append(args, instance.SemiSyncReplicaEnabled)
		args = append(args, instance.InstanceAlias)
		args = append(args, instance.LastDiscoveryLatency.Nanoseconds())
	}

	sql, err := mkInsertOdku("database_instance", columns, values, len(instances), insertIgnore)
	if err != nil {
		return sql, args, log.Errorf("Failed to build query: %v", err)
	}

	return sql, args, nil
}

// writeManyInstances stores instances in the orchestrator backend
func writeManyInstances(instances []*Instance, instanceWasActuallyFound bool, updateLastSeen bool) error {
	if len(instances) == 0 {
		return nil // nothing to write
	}

	writeInstances := [](*Instance){}
	for _, instance := range instances {
		if !InstanceIsForgotten(&instance.Key) {
			writeInstances = append(writeInstances, instance)
		}
	}
	sql, args, err := mkInsertOdkuForInstances(writeInstances, instanceWasActuallyFound, updateLastSeen)
	if err != nil {
		return err
	}

	if _, err := db.ExecOrchestrator(sql, args...); err != nil {
		return err
	}
	return nil
}

type instanceUpdateObject struct {
	instance                 *Instance
	instanceWasActuallyFound bool
	lastError                error
}

// instances sorter by instanceKey
type byInstanceKey []*Instance

func (a byInstanceKey) Len() int           { return len(a) }
func (a byInstanceKey) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byInstanceKey) Less(i, j int) bool { return a[i].Key.SmallerThan(&a[j].Key) }

var instanceWriteBuffer chan instanceUpdateObject
var forceFlushInstanceWriteBuffer = make(chan bool)

func enqueueInstanceWrite(instance *Instance, instanceWasActuallyFound bool, lastError error) {
	if len(instanceWriteBuffer) == config.Config.InstanceWriteBufferSize {
		// Signal the "flushing" gorouting that there's work.
		// We prefer doing all bulk flushes from one goroutine.
		forceFlushInstanceWriteBuffer <- true
	}
	instanceWriteBuffer <- instanceUpdateObject{instance, instanceWasActuallyFound, lastError}
}

// flushInstanceWriteBuffer saves enqueued instances to Orchestrator Db
func flushInstanceWriteBuffer() {
	var instances []*Instance
	var lastseen []*Instance // instances to update with last_seen field

	if len(instanceWriteBuffer) == 0 {
		return
	}

	for i := 0; i < len(instanceWriteBuffer); i++ {
		upd := <-instanceWriteBuffer
		if upd.instanceWasActuallyFound && upd.lastError == nil {
			lastseen = append(lastseen, upd.instance)
		} else {
			instances = append(instances, upd.instance)
			log.Debugf("flushInstanceWriteBuffer: will not update database_instance.last_seen due to error: %+v", upd.lastError)
		}
	}
	// sort instances by instanceKey (table pk) to make locking predictable
	sort.Sort(byInstanceKey(instances))
	sort.Sort(byInstanceKey(lastseen))

	writeFunc := func() error {
		err := writeManyInstances(instances, true, false)
		if err != nil {
			return log.Errorf("flushInstanceWriteBuffer writemany: %v", err)
		}
		err = writeManyInstances(lastseen, true, true)
		if err != nil {
			return log.Errorf("flushInstanceWriteBuffer last_seen: %v", err)
		}

		writeInstanceCounter.Inc(int64(len(instances) + len(lastseen)))
		return nil
	}
	err := ExecDBWriteFunc(writeFunc)
	if err != nil {
		log.Errorf("flushInstanceWriteBuffer: %v", err)
	}
}

// WriteInstance stores an instance in the orchestrator backend
func WriteInstance(instance *Instance, instanceWasActuallyFound bool, lastError error) error {
	if lastError != nil {
		log.Debugf("writeInstance: will not update database_instance due to error: %+v", lastError)
		return nil
	}
	return writeManyInstances([]*Instance{instance}, instanceWasActuallyFound, true)
}

// UpdateInstanceLastChecked updates the last_check timestamp in the orchestrator backed database
// for a given instance
func UpdateInstanceLastChecked(instanceKey *InstanceKey, partialSuccess bool) error {
	writeFunc := func() error {
		_, err := db.ExecOrchestrator(`
        	update
        		database_instance
        	set
						last_checked = NOW(),
						last_check_partial_success = ?
			where
				hostname = ?
				and port = ?`,
			partialSuccess,
			instanceKey.Hostname,
			instanceKey.Port,
		)
		return log.Errore(err)
	}
	return ExecDBWriteFunc(writeFunc)
}

// UpdateInstanceLastAttemptedCheck updates the last_attempted_check timestamp in the orchestrator backed database
// for a given instance.
// This is used as a failsafe mechanism in case access to the instance gets hung (it happens), in which case
// the entire ReadTopology gets stuck (and no, connection timeout nor driver timeouts don't help. Don't look at me,
// the world is a harsh place to live in).
// And so we make sure to note down *before* we even attempt to access the instance; and this raises a red flag when we
// wish to access the instance again: if last_attempted_check is *newer* than last_checked, that's bad news and means
// we have a "hanging" issue.
func UpdateInstanceLastAttemptedCheck(instanceKey *InstanceKey) error {
	writeFunc := func() error {
		_, err := db.ExecOrchestrator(`
    	update
    		database_instance
    	set
    		last_attempted_check = NOW()
			where
				hostname = ?
				and port = ?`,
			instanceKey.Hostname,
			instanceKey.Port,
		)
		return log.Errore(err)
	}
	return ExecDBWriteFunc(writeFunc)
}

func InstanceIsForgotten(instanceKey *InstanceKey) bool {
	_, found := forgetInstanceKeys.Get(instanceKey.StringCode())
	return found
}

// ForgetInstance removes an instance entry from the orchestrator backed database.
// It may be auto-rediscovered through topology or requested for discovery by multiple means.
func ForgetInstance(instanceKey *InstanceKey) error {
	forgetInstanceKeys.Set(instanceKey.StringCode(), true, cache.DefaultExpiration)
	_, err := db.ExecOrchestrator(`
			delete
				from database_instance
			where
				hostname = ? and port = ?`,
		instanceKey.Hostname,
		instanceKey.Port,
	)
	AuditOperation("forget", instanceKey, "")
	return err
}

// ForgetInstance removes an instance entry from the orchestrator backed database.
// It may be auto-rediscovered through topology or requested for discovery by multiple means.
func ForgetCluster(clusterName string) error {
	clusterInstances, err := ReadClusterInstances(clusterName)
	if err != nil {
		return err
	}
	if len(clusterInstances) == 0 {
		return nil
	}
	for _, instance := range clusterInstances {
		forgetInstanceKeys.Set(instance.Key.StringCode(), true, cache.DefaultExpiration)
		AuditOperation("forget", &instance.Key, "")
	}
	_, err = db.ExecOrchestrator(`
			delete
				from database_instance
			where
				cluster_name = ?`,
		clusterName,
	)
	return err
}

// ForgetLongUnseenInstances will remove entries of all instacnes that have long since been last seen.
func ForgetLongUnseenInstances() error {
	sqlResult, err := db.ExecOrchestrator(`
			delete
				from database_instance
			where
				last_seen < NOW() - interval ? hour`,
		config.Config.UnseenInstanceForgetHours,
	)
	if err != nil {
		return log.Errore(err)
	}
	rows, err := sqlResult.RowsAffected()
	if err != nil {
		return log.Errore(err)
	}
	AuditOperation("forget-unseen", nil, fmt.Sprintf("Forgotten instances: %d", rows))
	return err
}

// SnapshotTopologies records topology graph for all existing topologies
func SnapshotTopologies() error {
	writeFunc := func() error {
		_, err := db.ExecOrchestrator(`
        	insert ignore into
        		database_instance_topology_history (snapshot_unix_timestamp,
        			hostname, port, main_host, main_port, cluster_name, version)
        	select
        		UNIX_TIMESTAMP(NOW()),
        		hostname, port, main_host, main_port, cluster_name, version
			from
				database_instance
				`,
		)
		if err != nil {
			return log.Errore(err)
		}

		return nil
	}
	return ExecDBWriteFunc(writeFunc)
}

// ReadHistoryClusterInstances reads (thin) instances from history
func ReadHistoryClusterInstances(clusterName string, historyTimestampPattern string) ([](*Instance), error) {
	instances := [](*Instance){}

	query := `
		select
			*
		from
			database_instance_topology_history
		where
			snapshot_unix_timestamp rlike ?
			and cluster_name = ?
		order by
			hostname, port`

	err := db.QueryOrchestrator(query, sqlutils.Args(historyTimestampPattern, clusterName), func(m sqlutils.RowMap) error {
		instance := NewInstance()

		instance.Key.Hostname = m.GetString("hostname")
		instance.Key.Port = m.GetInt("port")
		instance.MainKey.Hostname = m.GetString("main_host")
		instance.MainKey.Port = m.GetInt("main_port")
		instance.ClusterName = m.GetString("cluster_name")

		instances = append(instances, instance)
		return nil
	})
	if err != nil {
		return instances, log.Errore(err)
	}
	return instances, err
}

// RegisterCandidateInstance markes a given instance as suggested for successoring a main in the event of failover.
func RegisterCandidateInstance(candidate *CandidateDatabaseInstance) error {
	args := sqlutils.Args(candidate.Hostname, candidate.Port, string(candidate.PromotionRule))
	lastSuggestedHint := "now()"
	if !candidate.LastSuggested.IsZero() {
		lastSuggestedHint = "?"
		args = append(args, candidate.LastSuggested)
	}
	query := fmt.Sprintf(`
			insert into candidate_database_instance (
					hostname,
					port,
					promotion_rule,
					last_suggested
				) values (?, ?, ?, %s)
				on duplicate key update
					hostname=values(hostname),
					port=values(port),
					last_suggested=now(),
					promotion_rule=values(promotion_rule)
			`, lastSuggestedHint)
	writeFunc := func() error {
		_, err := db.ExecOrchestrator(query, args...)
		AuditOperation("register-candidate", candidate.Key(), string(candidate.PromotionRule))
		return log.Errore(err)
	}
	return ExecDBWriteFunc(writeFunc)
}

// ExpireCandidateInstances removes stale main candidate suggestions.
func ExpireCandidateInstances() error {
	writeFunc := func() error {
		_, err := db.ExecOrchestrator(`
        	delete from candidate_database_instance
				where last_suggested < NOW() - INTERVAL ? MINUTE
				`, config.Config.CandidateInstanceExpireMinutes,
		)
		if err != nil {
			return log.Errore(err)
		}

		return nil
	}
	return ExecDBWriteFunc(writeFunc)
}

// RecordInstanceCoordinatesHistory snapshots the binlog coordinates of instances
func RecordInstanceCoordinatesHistory() error {
	{
		writeFunc := func() error {
			_, err := db.ExecOrchestrator(`
        	delete from database_instance_coordinates_history
			where
				recorded_timestamp < NOW() - INTERVAL ? MINUTE
				`, (config.PseudoGTIDCoordinatesHistoryHeuristicMinutes + 2),
			)
			return log.Errore(err)
		}
		ExecDBWriteFunc(writeFunc)
	}
	writeFunc := func() error {
		_, err := db.ExecOrchestrator(`
    	insert into
    		database_instance_coordinates_history (
					hostname, port,	last_seen, recorded_timestamp,
					binary_log_file, binary_log_pos, relay_log_file, relay_log_pos
				)
    	select
    		hostname, port, last_seen, NOW(),
				binary_log_file, binary_log_pos, relay_log_file, relay_log_pos
			from
				database_instance
			where
				binary_log_file != ''
				OR relay_log_file != ''
				`,
		)
		return log.Errore(err)
	}
	return ExecDBWriteFunc(writeFunc)
}

// GetHeuristiclyRecentCoordinatesForInstance returns valid and reasonably recent coordinates for given instance.
func GetHeuristiclyRecentCoordinatesForInstance(instanceKey *InstanceKey) (selfCoordinates *BinlogCoordinates, relayLogCoordinates *BinlogCoordinates, err error) {
	query := `
		select
			binary_log_file, binary_log_pos, relay_log_file, relay_log_pos
		from
			database_instance_coordinates_history
		where
			hostname = ?
			and port = ?
			and recorded_timestamp <= NOW() - INTERVAL ? MINUTE
		order by
			recorded_timestamp desc
			limit 1
			`
	err = db.QueryOrchestrator(query, sqlutils.Args(instanceKey.Hostname, instanceKey.Port, config.PseudoGTIDCoordinatesHistoryHeuristicMinutes), func(m sqlutils.RowMap) error {
		selfCoordinates = &BinlogCoordinates{LogFile: m.GetString("binary_log_file"), LogPos: m.GetInt64("binary_log_pos")}
		relayLogCoordinates = &BinlogCoordinates{LogFile: m.GetString("relay_log_file"), LogPos: m.GetInt64("relay_log_pos")}

		return nil
	})
	return selfCoordinates, relayLogCoordinates, err
}

// GetPreviousKnownRelayLogCoordinatesForInstance returns known relay log coordinates, that are not the
// exact current coordinates
func GetPreviousKnownRelayLogCoordinatesForInstance(instance *Instance) (relayLogCoordinates *BinlogCoordinates, err error) {
	query := `
		select
			relay_log_file, relay_log_pos
		from
			database_instance_coordinates_history
		where
			hostname = ?
			and port = ?
			and (relay_log_file, relay_log_pos) < (?, ?)
			and relay_log_file != ''
			and relay_log_pos != 0
		order by
			recorded_timestamp desc
			limit 1
			`
	err = db.QueryOrchestrator(query, sqlutils.Args(
		instance.Key.Hostname,
		instance.Key.Port,
		instance.RelaylogCoordinates.LogFile,
		instance.RelaylogCoordinates.LogPos,
	), func(m sqlutils.RowMap) error {
		relayLogCoordinates = &BinlogCoordinates{LogFile: m.GetString("relay_log_file"), LogPos: m.GetInt64("relay_log_pos")}

		return nil
	})
	return relayLogCoordinates, err
}

// ResetInstanceRelaylogCoordinatesHistory forgets about the history of an instance. This action is desirable
// when relay logs become obsolete or irrelevant. Such is the case on `CHANGE MASTER TO`: servers gets compeltely
// new relay logs.
func ResetInstanceRelaylogCoordinatesHistory(instanceKey *InstanceKey) error {
	writeFunc := func() error {
		_, err := db.ExecOrchestrator(`
			update database_instance_coordinates_history
				set relay_log_file='', relay_log_pos=0
			where
				hostname=? and port=?
				`, instanceKey.Hostname, instanceKey.Port,
		)
		return log.Errore(err)
	}
	return ExecDBWriteFunc(writeFunc)
}

func ExpireInstanceBinlogFileHistory() error {
	return ExpireTableData("database_instance_binlog_files_history", "last_seen")
}

// FigureClusterName will make a best effort to deduce a cluster name using either a given alias
// or an instanceKey. First attempt is at alias, and if that doesn't work, we try instanceKey.
func FigureClusterName(clusterHint string, instanceKey *InstanceKey, thisInstanceKey *InstanceKey) (clusterName string, err error) {
	// Look for exact matches, first.

	// Exact cluster name match:
	if clusterInfo, err := ReadClusterInfo(clusterHint); err == nil && clusterInfo != nil {
		return clusterInfo.ClusterName, nil
	}
	// Exact cluster alias match:
	if clustersInfo, err := ReadClustersInfo(""); err == nil {
		for _, clusterInfo := range clustersInfo {
			if clusterInfo.ClusterAlias == clusterHint {
				return clusterInfo.ClusterName, nil
			}
		}
	}
	clusterByInstanceKey := func(instanceKey *InstanceKey) (hasResult bool, clusterName string, err error) {
		if instanceKey == nil {
			return false, "", nil
		}
		instance, _, err := ReadInstance(instanceKey)
		if err != nil {
			return true, clusterName, log.Errore(err)
		}
		if instance != nil {
			if instance.ClusterName == "" {
				return true, clusterName, log.Errorf("Unable to determine cluster name")
			}
			return true, instance.ClusterName, nil
		}
		return false, "", nil
	}
	// exact instance key:
	if hasResult, clusterName, err := clusterByInstanceKey(instanceKey); hasResult {
		return clusterName, err
	}
	// fuzzy instance key:
	if hasResult, clusterName, err := clusterByInstanceKey(ReadFuzzyInstanceKeyIfPossible(instanceKey)); hasResult {
		return clusterName, err
	}
	//  Let's see about _this_ instance
	if hasResult, clusterName, err := clusterByInstanceKey(thisInstanceKey); hasResult {
		return clusterName, err
	}
	return clusterName, log.Errorf("Unable to determine cluster name")
}

// FigureInstanceKey tries to figure out a key
func FigureInstanceKey(instanceKey *InstanceKey, thisInstanceKey *InstanceKey) (*InstanceKey, error) {
	if figuredKey := ReadFuzzyInstanceKeyIfPossible(instanceKey); figuredKey != nil {
		return figuredKey, nil
	}
	figuredKey := thisInstanceKey
	if figuredKey == nil {
		return nil, log.Errorf("Cannot deduce instance %+v", instanceKey)
	}
	return figuredKey, nil
}

// RegisterInjectedPseudoGTID
func RegisterInjectedPseudoGTID(clusterName string) error {
	query := `
			insert into cluster_injected_pseudo_gtid (
					cluster_name,
					time_injected
				) values (?, now())
				on duplicate key update
					cluster_name=values(cluster_name),
					time_injected=now()
				`
	args := sqlutils.Args(clusterName)
	writeFunc := func() error {
		_, err := db.ExecOrchestrator(query, args...)
		if err == nil {
			clusterInjectedPseudoGTIDCache.Set(clusterName, true, cache.DefaultExpiration)
		}
		return log.Errore(err)
	}
	return ExecDBWriteFunc(writeFunc)
}

// ExpireInjectedPseudoGTID
func ExpireInjectedPseudoGTID() error {
	writeFunc := func() error {
		_, err := db.ExecOrchestrator(`
				delete from cluster_injected_pseudo_gtid
				where time_injected < NOW() - INTERVAL ? MINUTE
				`, config.PseudoGTIDExpireMinutes,
		)
		return log.Errore(err)
	}
	return ExecDBWriteFunc(writeFunc)
}

// isInjectedPseudoGTID reads from backend DB / cache
func isInjectedPseudoGTID(clusterName string) (injected bool, err error) {
	if injectedValue, found := clusterInjectedPseudoGTIDCache.Get(clusterName); found {
		return injectedValue.(bool), err
	}
	query := `
			select
					count(*) as is_injected
				from
					cluster_injected_pseudo_gtid
				where
					cluster_name = ?
			`
	err = db.QueryOrchestrator(query, sqlutils.Args(clusterName), func(m sqlutils.RowMap) error {
		injected = m.GetBool("is_injected")
		return nil
	})
	clusterInjectedPseudoGTIDCache.Set(clusterName, injected, cache.DefaultExpiration)
	return injected, log.Errore(err)
}
