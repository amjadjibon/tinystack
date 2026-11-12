package rds

import (
	"context"
	"slices"
	"strconv"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	mysqlFamilyVersion = "8.0"
	parameterInSync    = "in-sync"
	parameterBoolean   = "boolean"
	parameterImmediate = "immediate"

	parameterPending       = "pending-reboot"
	parameterInteger       = "integer"
	parameterString        = "string"
	parameterFloat         = "float"
	parameterOff           = "off"
	parameterBooleanValues = "0,1,on,off"
)

const kindClusterParameter = "cluster-pg"

const (
	docDB40         = "4.0"
	docDB50         = "5.0"
	parameterSwitch = "enabled,disabled"
	parameterNo     = "disabled"
	parameterFlag   = "0-1"
)

type parameterDefinition struct {
	name, value, dataType, allowed string
	static, immutable              bool
}

// Pinned portable engine parameters; AWS-specific extension settings fail explicitly.
var postgresParameters = []parameterDefinition{
	{"max_connections", "100", parameterInteger, "6-262143", true, false},
	{"shared_buffers", "16384", parameterInteger, "16-1073741823", true, false},
	{"work_mem", "4096", parameterInteger, "64-2147483647", false, false},
	{"maintenance_work_mem", "65536", parameterInteger, "1024-2147483647", false, false},
	{"effective_cache_size", "524288", parameterInteger, "1-2147483647", false, false},
	{"statement_timeout", "0", parameterInteger, "0-2147483647", false, false},
	{"idle_in_transaction_session_timeout", "0", parameterInteger, "0-2147483647", false, false},
	{"log_min_duration_statement", "-1", parameterInteger, "-1-2147483647", false, false},
	{"log_statement", "none", parameterString, "none,ddl,mod,all", false, false},
	{"log_connections", parameterOff, parameterBoolean, parameterBooleanValues, false, false},
	{"log_disconnections", parameterOff, parameterBoolean, parameterBooleanValues, false, false},
	{"log_lock_waits", parameterOff, parameterBoolean, parameterBooleanValues, false, false},
	{"random_page_cost", "4", parameterFloat, "0-1000", false, false},
	{"autovacuum", "on", parameterBoolean, parameterBooleanValues, false, false},
	{"autovacuum_max_workers", "3", parameterInteger, "1-262143", true, false},
	{"timezone", "UTC", parameterString, "UTC,GMT,Asia/Kuala_Lumpur,America/New_York,Europe/London", false, false},
	{"shared_preload_libraries", "", parameterString, ",pg_stat_statements", true, false},
	{"client_encoding", "UTF8", parameterString, "UTF8,LATIN1,SQL_ASCII", false, false},
	{"password_encryption", "scram-sha-256", parameterString, "md5,scram-sha-256", false, false},
	{"max_worker_processes", "8", parameterInteger, "0-262143", true, false},
	{"server_version", "", parameterString, "", true, true},
}

var mysqlParameters = []parameterDefinition{
	{"max_connections", "151", parameterInteger, "1-100000", false, false},
	{"character_set_server", "utf8mb4", parameterString, "utf8mb4,utf8,latin1,ascii", false, false},
	{
		"collation_server",
		"utf8mb4_general_ci",
		parameterString,
		"utf8mb4_general_ci,utf8mb4_unicode_ci,utf8mb4_0900_ai_ci,utf8_general_ci,latin1_swedish_ci",
		false,
		false,
	},
	{"slow_query_log", "0", parameterBoolean, "0,1,ON,OFF", false, false},
	{"general_log", "0", parameterBoolean, "0,1,ON,OFF", false, false},
	{"long_query_time", "10", parameterFloat, "0-31536000", false, false},
	{"wait_timeout", "28800", parameterInteger, "1-31536000", false, false},
	{"interactive_timeout", "28800", parameterInteger, "1-31536000", false, false},
	{"max_allowed_packet", "67108864", parameterInteger, "1024-1073741824", false, false},
	{"innodb_buffer_pool_size", "134217728", parameterInteger, "5242880-1099511627776", true, false},
	{"innodb_flush_log_at_trx_commit", "1", parameterInteger, "0-2", false, false},
	{"innodb_lock_wait_timeout", "50", parameterInteger, "1-1073741824", false, false},
	{"sql_mode", "STRICT_TRANS_TABLES", parameterString, "", false, false},
	{"time_zone", "SYSTEM", parameterString, "SYSTEM,+00:00,UTC", false, false},
	{"log_output", "FILE", parameterString, "FILE,TABLE,NONE", false, false},
	{"lower_case_table_names", "0", parameterInteger, "0-2", true, false},
	{"version", "", parameterString, "", true, true},
}

// DocumentDB cluster parameters. TLS 1.2+/1.3+ need engine 4.0 and FIPS needs 5.0.
// https://docs.aws.amazon.com/documentdb/latest/developerguide/cluster_parameter_groups-list_of_parameters.html
func docDBParameters(version string) []parameterDefinition {
	tls := parameterSwitch
	switch version {
	case docDB40:
		tls += ",tls1.2+,tls1.3+"
	case docDB50:
		tls += ",tls1.2+,tls1.3+,fips-140-3"
	}

	return []parameterDefinition{
		{"audit_logs", parameterNo, parameterString, "enabled,disabled,ddl,dml_read,dml_write,all,none", false, false},
		{"profiler", parameterNo, parameterString, parameterSwitch, false, false},
		{"profiler_sampling_rate", "1.0", parameterFloat, "0.0-1.0", false, false},
		{"profiler_threshold_ms", "100", parameterInteger, "50-2147483646", false, false},
		{"tls", "enabled", parameterString, tls, true, false},
		{"ttl_monitor", "enabled", parameterString, parameterSwitch, false, false},
	}
}

// Neptune cluster parameters; https://docs.aws.amazon.com/neptune/latest/userguide/parameters.html
var neptuneParameters = []parameterDefinition{
	{"neptune_enable_audit_log", "0", parameterInteger, parameterFlag, true, false},
	{"neptune_enable_slow_query_log", parameterNo, parameterString, "info,debug,disabled", false, false},
	{"neptune_lookup_cache", "0", parameterInteger, parameterFlag, true, false},
	{"neptune_query_timeout", "120000", parameterInteger, "10-2147483647", true, false},
	{"neptune_streams", "0", parameterInteger, parameterFlag, true, false},
	{"neptune_streams_expiry_days", "7", parameterInteger, "1-90", true, false},
}

// Neptune instance parameters; all are static.
var neptuneInstanceParameters = []parameterDefinition{
	{"neptune_dfe_query_engine", "viaQueryHint", parameterString, "enabled,viaQueryHint", true, false},
	{"neptune_query_timeout", "120000", parameterInteger, "10-2147483647", true, false},
	{"neptune_result_cache", "0", parameterInteger, parameterFlag, true, false},
}

// The only values the Docker engines can honour for parameters they do not implement: community MongoDB has no
// TLS certificates or auditing, and Gremlin Server has no audit log, slow-query log or streams.
var dockerParameterValues = map[string][]string{
	"audit_logs":                    {parameterNo, "none"},
	"tls":                           {parameterNo},
	"neptune_enable_audit_log":      {"0"},
	"neptune_enable_slow_query_log": {parameterNo},
	"neptune_streams":               {"0"},
}

func parameterCatalog(family string, cluster bool) ([]parameterDefinition, error) {
	engine := ""
	version := ""
	engines := []string{EnginePostgres, EngineMySQL, EngineMariaDB, EngineNeptune}
	if cluster {
		engines = []string{EngineAuroraPostgres, EngineAuroraMySQL, EngineDocDB, EngineNeptune}
	}

	for _, e := range engines {
		if strings.HasPrefix(family, e) {
			engine, version = e, strings.TrimPrefix(family, e)
			break
		}
	}

	if engine == "" || version == "" {
		return nil, invalid("Unsupported DBParameterGroupFamily")
	}

	if cluster {
		supported := slices.Contains([]string{"15", "16", "17"}, version)
		switch engine {
		case EngineAuroraMySQL:
			supported = version == mysqlFamilyVersion
		case EngineDocDB:
			supported = slices.Contains([]string{"3.6", docDB40, docDB50}, version)
		case EngineNeptune:
			supported = slices.Contains([]string{"1.2", "1.3"}, version)
		}

		if !supported {
			return nil, invalid("Unsupported DBParameterGroupFamily")
		}
	} else if engine == EngineNeptune {
		if version != "1.2" && version != "1.3" {
			return nil, invalid("Unsupported DBParameterGroupFamily")
		}

		return neptuneInstanceParameters, nil
	} else if _, _, _, err := Engine(engine, version); err != nil {
		return nil, err
	}

	switch engine {
	case EnginePostgres, EngineAuroraPostgres:
		return postgresParameters, nil
	case EngineDocDB:
		return docDBParameters(version), nil
	case EngineNeptune:
		return neptuneParameters, nil
	}

	return mysqlParameters, nil
}

func parameterFamily(r *record) string {
	if r.ClusterParameter != nil {
		return common.Value(r.ClusterParameter.DBParameterGroupFamily)
	}

	return common.Value(r.Parameter.DBParameterGroupFamily)
}

func defaultParameterGroup(scope request.Scope, kind, name string) (*record, error) {
	family := strings.TrimPrefix(name, "default.")
	if _, err := parameterCatalog(family, kind == kindClusterParameter); err != nil {
		return nil, err
	}

	r := &record{Name: name, Kind: kind}
	if kind == kindClusterParameter {
		r.ClusterParameter = &api.DBClusterParameterGroup{
			DBClusterParameterGroupName: new(name),
			DBParameterGroupFamily:      new(family),
			Description:                 new("Default parameter group"),
			DBClusterParameterGroupArn:  new(arn(scope, kind, name)),
		}
	} else {
		r.Parameter = &api.DBParameterGroup{
			DBParameterGroupName:   new(name),
			DBParameterGroupFamily: new(family),
			Description:            new("Default parameter group"),
			DBParameterGroupArn:    new(arn(scope, kind, name)),
		}
	}

	return r, nil
}

func (s *Service) describeParameters(
	ctx context.Context,
	kind, name, family, source string,
	marker *string,
	maximum *int64,
) ([]api.Parameter, *string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	r := &record{}
	if name != "" {
		err = s.store.View(
			ctx,
			func(tx storage.ReadTx) error { var e error; r, e = readRecord(tx, scope, kind, name); return e },
		)
		if err != nil {
			return nil, nil, err
		}

		family = parameterFamily(r)
	}

	definitions, err := parameterCatalog(family, kind == kindClusterParameter)
	if err != nil {
		return nil, nil, err
	}

	if source != "" && source != "user" && source != "engine-default" && source != "system" {
		return nil, nil, invalid("Invalid Source")
	}

	parameters := []api.Parameter{}
	for _, d := range definitions {
		value, src := d.value, "engine-default"
		method := parameterPending
		if p, ok := r.Parameters[d.name]; ok {
			value, src, method = common.Value(p.ParameterValue), "user", common.Value(p.ApplyMethod)
		}

		if source != "" && source != src {
			continue
		}

		applyType := "dynamic"
		if d.static {
			applyType = "static"
		}

		parameters = append(
			parameters,
			api.Parameter{
				ParameterName:  new(d.name),
				ParameterValue: new(value),
				Description:    new(d.name),
				Source:         new(src),
				ApplyType:      new(applyType),
				DataType:       new(d.dataType),
				AllowedValues:  new(d.allowed),
				IsModifiable:   new(!d.immutable),
				ApplyMethod:    new(method),
			},
		)
	}

	slices.SortFunc(parameters, func(a, b api.Parameter) int {
		return strings.Compare(common.Value(a.ParameterName), common.Value(b.ParameterName))
	})
	limit := int(common.Value(common.OrElse(maximum, new(int64(100)))))
	if limit < 20 || limit > 100 {
		return nil, nil, invalid("MaxRecords must be between 20 and 100")
	}

	return common.Page(
		parameters,
		func(p api.Parameter) string { return common.Value(p.ParameterName) },
		marker,
		limit,
		invalid("Invalid Marker"),
	)
}

func validParameterValue(d parameterDefinition, value string) bool {
	if d.dataType == parameterInteger || d.dataType == parameterFloat {
		split := strings.Index(d.allowed[1:], "-") + 1
		low, _ := strconv.ParseFloat(d.allowed[:split], 64)
		high, _ := strconv.ParseFloat(d.allowed[split+1:], 64)
		v, err := strconv.ParseFloat(value, 64)
		if d.dataType == parameterInteger {
			if _, e := strconv.ParseInt(value, 10, 64); e != nil {
				return false
			}
		}

		return err == nil && v >= low && v <= high
	}

	if d.allowed != "" {
		return slices.Contains(strings.Split(d.allowed, ","), value)
	}

	// SQL mode is a comma-separated set of engine options.
	for _, v := range strings.Split(value, ",") {
		if !slices.Contains(
			[]string{
				"",
				"STRICT_TRANS_TABLES",
				"STRICT_ALL_TABLES",
				"NO_ZERO_DATE",
				"NO_ZERO_IN_DATE",
				"ERROR_FOR_DIVISION_BY_ZERO",
				"NO_ENGINE_SUBSTITUTION",
				"ONLY_FULL_GROUP_BY",
				"ANSI_QUOTES",
				"NO_BACKSLASH_ESCAPES",
			},
			v,
		) {
			return false
		}
	}

	return true
}

func (s *Service) alterParameters(
	ctx context.Context,
	kind, name string,
	parameters []api.Parameter,
	reset, all bool,
) error {
	if strings.HasPrefix(strings.ToLower(name), "default.") {
		return invalid("Default parameter groups are immutable")
	}

	if len(parameters) > 20 || (!all && len(parameters) == 0) || (all && len(parameters) != 0) {
		return invalid("Supply 1 to 20 parameters, or ResetAllParameters alone")
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	previouslyPending := map[string]bool{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readRecord(tx, scope, kind, name)
		if e != nil {
			return e
		}

		definitions, e := parameterCatalog(parameterFamily(r), kind == kindClusterParameter)
		if e != nil {
			return e
		}

		if r.Parameters == nil {
			r.Parameters = map[string]api.Parameter{}
		}

		seen := map[string]bool{}
		for _, p := range parameters {
			pname := common.Value(p.ParameterName)
			i := slices.IndexFunc(definitions, func(d parameterDefinition) bool { return d.name == pname })
			if i < 0 || seen[pname] {
				return invalid("Unknown or duplicate parameter: " + pname)
			}

			seen[pname] = true
			d := definitions[i]
			method := common.Value(p.ApplyMethod)
			if d.immutable || (method != parameterImmediate && method != parameterPending) ||
				(d.static && method == parameterImmediate) {
				return invalid("Invalid ApplyMethod or non-modifiable parameter: " + pname)
			}

			if !reset && (p.ParameterValue == nil || !validParameterValue(d, *p.ParameterValue)) {
				return invalid("Invalid value for parameter: " + pname)
			}

			if honoured, limited := dockerParameterValues[pname]; !reset && s.runtime != nil && limited &&
				!slices.Contains(honoured, *p.ParameterValue) {
				return invalid("Docker engines support only " + strings.Join(honoured, " or ") + " for " + pname)
			}

			// Docker Neptune members share their cluster's Gremlin Server, so only cluster groups configure it.
			if !reset && s.runtime != nil && kind == kindParameter &&
				strings.HasPrefix(parameterFamily(r), EngineNeptune) && *p.ParameterValue != d.value {
				return invalid("Docker Neptune members share one server; set " + pname + " in the cluster group")
			}

			if reset {
				delete(r.Parameters, pname)
			} else {
				r.Parameters[pname] = api.Parameter{
					ParameterName:  p.ParameterName,
					ParameterValue: p.ParameterValue,
					ApplyMethod:    p.ApplyMethod,
				}
			}
		}

		if all {
			r.Parameters = nil
		}

		if writeErr := writeRecord(tx, scope, r); writeErr != nil {
			return writeErr
		}

		// Runtime changes are applied on restart; report them rather than silently ignoring them.
		instances, e := storage.Records[record](tx, scope.Prefix(serviceName, kindDB))
		if e != nil {
			return e
		}

		for _, db := range instances {
			for i, p := range db.Instance.DBParameterGroups {
				if common.Value(p.DBParameterGroupName) != r.Name || kind != kindParameter {
					continue
				}

				previouslyPending[db.Name] = common.Value(p.ParameterApplyStatus) == parameterPending
				status := parameterInSync
				// Cluster members have no instance engine to restart.
				if s.runtime != nil && db.Instance.DBClusterIdentifier == nil {
					status = parameterPending
				}

				db.Instance.DBParameterGroups[i].ParameterApplyStatus = new(status)
				if e := writeRecord(tx, scope, db); e != nil {
					return e
				}
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	if !reset && kind == kindParameter {
		s.applyDynamicParameters(ctx, scope, strings.ToLower(name), parameters, previouslyPending)
	}

	return nil
}

// Optional runtime capability: engines without a live path retain pending-reboot.
type parameterRuntime interface {
	ApplyParameters(context.Context, *Spec, map[string]string) error
}

func (s *Service) applyDynamicParameters(
	ctx context.Context,
	scope request.Scope,
	name string,
	parameters []api.Parameter,
	previouslyPending map[string]bool,
) {
	rt, ok := s.runtime.(parameterRuntime)
	if !ok {
		return
	}

	for _, p := range parameters {
		if common.Value(p.ApplyMethod) != parameterImmediate {
			return
		}
	}

	items, err := s.list(ctx, scope, kindDB, "")
	if err != nil {
		return
	}

	values := map[string]string{}
	for _, p := range parameters {
		values[common.Value(p.ParameterName)] = common.Value(p.ParameterValue)
	}

	for _, r := range items {
		if r.Instance.DBClusterIdentifier != nil || common.Value(r.Instance.DBInstanceStatus) != available {
			continue
		}

		for i, p := range r.Instance.DBParameterGroups {
			if common.Value(p.DBParameterGroupName) != name {
				continue
			}

			if applyErr := rt.ApplyParameters(ctx, s.spec(ctx, scope, r), values); applyErr != nil {
				continue
			}

			if previouslyPending[r.Name] {
				continue
			}
			r.Instance.DBParameterGroups[i].ParameterApplyStatus = new(parameterInSync)
			_ = s.store.Update(ctx, func(tx storage.WriteTx) error { return writeRecord(tx, scope, r) })
		}
	}
}

func clusterFamily(engine, version string) string {
	switch engine {
	case EngineAuroraMySQL:
		return engine + mysqlFamilyVersion
	case EngineDocDB:
		return engine + version
	case EngineNeptune:
		return engine + strings.Join(strings.Split(version, ".")[:2], ".")
	}

	return engine + strings.Split(version, ".")[0]
}

func (s *Service) runtimeParameters(ctx context.Context, scope request.Scope, r *record) map[string]string {
	kind, name := kindParameter, ""
	if r.Cluster != nil {
		kind, name = kindClusterParameter, common.Value(r.Cluster.DBClusterParameterGroup)
	} else if len(r.Instance.DBParameterGroups) > 0 {
		name = common.Value(r.Instance.DBParameterGroups[0].DBParameterGroupName)
	}

	values := map[string]string{}
	if name == "" {
		return values
	}

	_ = s.store.View(ctx, func(tx storage.ReadTx) error {
		group, err := readRecord(tx, scope, kind, name)
		if err != nil {
			return err
		}

		for key, p := range group.Parameters {
			values[key] = common.Value(p.ParameterValue)
		}

		return nil
	})
	return values
}
