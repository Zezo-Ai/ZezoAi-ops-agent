// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package apps

import (
	"context"
	"strings"

	"github.com/GoogleCloudPlatform/ops-agent/confgenerator"
	"github.com/GoogleCloudPlatform/ops-agent/confgenerator/fluentbit"
	"github.com/GoogleCloudPlatform/ops-agent/confgenerator/otel"
	"github.com/GoogleCloudPlatform/ops-agent/internal/secret"
)

type MetricsReceiverPostgresql struct {
	confgenerator.ConfigComponent `yaml:",inline"`

	confgenerator.MetricsReceiverShared    `yaml:",inline"`
	confgenerator.MetricsReceiverSharedTLS `yaml:",inline"`

	Endpoint string `yaml:"endpoint" validate:"omitempty,hostname_port|startswith=/"`

	Password  secret.String `yaml:"password" validate:"omitempty"`
	Username  string        `yaml:"username" validate:"omitempty"`
	Databases []string      `yaml:"databases" validate:"omitempty"`
}

// Actual socket is /var/run/postgresql/.s.PGSQL.5432 but the lib/pq go module used by
// the underlying receiver expects it like this
const defaultPostgresqlUnixEndpoint = "var/run/postgresql/:5432"

func (r MetricsReceiverPostgresql) Type() string {
	return "postgresql"
}

func (r MetricsReceiverPostgresql) Pipelines(_ context.Context) ([]otel.ReceiverPipeline, error) {
	transport := "tcp"
	if r.Endpoint == "" {
		transport = "unix"
		r.Endpoint = defaultPostgresqlUnixEndpoint
	} else if strings.HasPrefix(r.Endpoint, "/") {
		transport = "unix"
		endpointParts := strings.Split(r.Endpoint, ".")
		r.Endpoint = strings.TrimLeft(endpointParts[0], "/") + ":" + endpointParts[len(endpointParts)-1]
	}

	cfg := map[string]interface{}{
		"collection_interval": r.CollectionIntervalString(),
		"endpoint":            r.Endpoint,
		"username":            r.Username,
		"password":            r.Password.SecretValue(),
		"transport":           transport,
		"metrics": map[string]any{
			"postgresql.wal.delay": map[string]any{
				"enabled": true,
			},
			"postgresql.wal.lag": map[string]any{
				"enabled": false,
			},
		},
	}

	if transport == "tcp" {
		cfg["tls"] = r.TLSConfig(true)
	}

	return []otel.ReceiverPipeline{{
		Receiver: otel.Component{
			Type:   "postgresql",
			Config: cfg,
		},
		Processors: map[string][]otel.Component{"metrics": {
			otel.NormalizeSums(),
			otel.TransformationMetrics(
				otel.FlattenResourceAttribute("postgresql.database.name", "database"),
				otel.FlattenResourceAttribute("postgresql.table.name", "table"),
				otel.FlattenResourceAttribute("postgresql.index.name", "index"),
				// As of version 0.89, the postgresql receiver supports a double-precision wal.lag metric replacement
				// the following two transforms convert it back to integer-precision wal.lag for backwards compatibility.
				// The two metrics are mutually exclusive so we do not need to worry about overwriting or removing the original wal.lag.
				otel.ConvertFloatToInt("postgresql.wal.delay"),
				otel.SetName("postgresql.wal.delay", "postgresql.wal.lag"),
				otel.SetScopeName("agent.googleapis.com/"+r.Type()),
				otel.SetScopeVersion("1.0"),
			),
			otel.MetricsTransform(
				otel.UpdateMetric("postgresql.bgwriter.duration",
					otel.ToggleScalarDataType,
				),
				otel.AddPrefix("workload.googleapis.com"),
			),
		}},
	}}, nil
}

func init() {
	confgenerator.MetricsReceiverTypes.RegisterType(func() confgenerator.MetricsReceiver { return &MetricsReceiverPostgresql{} })
}

type LoggingProcessorPostgresql struct {
	confgenerator.ConfigComponent `yaml:",inline"`
}

func (LoggingProcessorPostgresql) Type() string {
	return "postgresql_general"
}

func (p LoggingProcessorPostgresql) Components(ctx context.Context, tag string, uid string) []fluentbit.Component {
	c := confgenerator.LoggingProcessorParseMultilineRegex{
		LoggingProcessorParseRegexComplex: confgenerator.LoggingProcessorParseRegexComplex{
			// Limited logging documentation: https://www.postgresql.org/docs/10/runtime-config-logging.html
			Parsers: []confgenerator.RegexParser{
				{
					// This parser matches most distributions' defaults by our testing
					// log_line_prefix = '%m [%p] '
					// log_line_prefix = '%m [%p] %q%u@%d '
					// Sample line: 2022-01-12 20:57:58.378 UTC [26241] LOG:  starting PostgreSQL 14.1 (Debian 14.1-1.pgdg100+1) on x86_64-pc-linux-gnu, compiled by gcc (Debian 8.3.0-6) 8.3.0, 64-bit
					// Sample line: 2022-01-12 20:59:25.169 UTC [27445] postgres@postgres FATAL:  Peer authentication failed for user "postgres"
					// Sample line: 2022-01-12 21:49:13.989 UTC [27836] postgres@postgres LOG:  duration: 1.074 ms  statement: select *
					//    from pg_database
					//    where 1=1;
					Regex: `^(?<time>\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}.\d{3,} \w+)\s*\[(?<tid>\d+)\](?:\s+(?<user>\S*)@(?<database>\S*))?\s*(?<level>\w+):\s+(?<message>[\s\S]*)`,
					Parser: confgenerator.ParserShared{
						TimeKey:    "time",
						TimeFormat: "%Y-%m-%d %H:%M:%S.%L %z",
						Types: map[string]string{
							"tid": "integer",
						},
					},
				},
				{
					// This parser matches the default log_line_prefix from SLES12 in our testing
					// log_line_prefix = '%m %d %u [%p]'
					// Sample line: 2024-05-30 15:34:26.572 UTC postgres postgres [23958]STATEMENT:  INSERT INTO
					//					test2 (id) VALUES('1970-01-01 00:00:00.123 UTC');
					Regex: `^(?<time>\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}.\d{3,} \w+)\s*(?:\s+(?<database>\S*)\s+(?<user>\S*))?\s*\[(?<tid>\d+)\]\s*(?<level>\w+):\s+(?<message>[\s\S]*)`,
					Parser: confgenerator.ParserShared{
						TimeKey:    "time",
						TimeFormat: "%Y-%m-%d %H:%M:%S.%L %z",
						Types: map[string]string{
							"tid": "integer",
						},
					},
				},
			},
		},
		Rules: []confgenerator.MultilineRule{
			{
				StateName: "start_state",
				NextState: "cont",
				Regex:     `^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}.\d{3,} \w+`,
			},
			{
				StateName: "cont",
				NextState: "cont",
				Regex:     `^(?!\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}.\d{3,} \w+)`,
			},
		},
	}.Components(ctx, tag, uid)

	// https://www.postgresql.org/docs/10/runtime-config-logging.html#RUNTIME-CONFIG-SEVERITY-LEVELS
	c = append(c,
		confgenerator.LoggingProcessorModifyFields{
			Fields: map[string]*confgenerator.ModifyField{
				"severity": {
					CopyFrom: "jsonPayload.level",
					MapValues: map[string]string{
						"DEBUG1":    "DEBUG",
						"DEBUG2":    "DEBUG",
						"DEBUG3":    "DEBUG",
						"DEBUG4":    "DEBUG",
						"DEBUG5":    "DEBUG",
						"DETAIL":    "DEBUG",
						"STATEMENT": "DEBUG",
						"INFO":      "INFO",
						"LOG":       "INFO",
						"NOTICE":    "INFO",
						"ERROR":     "ERROR",
						"WARNING":   "WARNING",
						"FATAL":     "CRITICAL",
						"PANIC":     "CRITICAL",
					},
					MapValuesExclusive: true,
				},
				InstrumentationSourceLabel: instrumentationSourceValue(p.Type()),
			},
		}.Components(ctx, tag, uid)...,
	)

	return c
}

type LoggingReceiverPostgresql struct {
	LoggingProcessorPostgresql `yaml:",inline"`
	ReceiverMixin              confgenerator.LoggingReceiverFilesMixin `yaml:",inline" validate:"structonly"`
}

func (r LoggingReceiverPostgresql) Components(ctx context.Context, tag string) []fluentbit.Component {
	if len(r.ReceiverMixin.IncludePaths) == 0 {
		r.ReceiverMixin.IncludePaths = []string{
			// Default log paths for Debian / Ubuntu
			"/var/log/postgresql/postgresql*.log",
			// Default log paths for SLES
			"/var/lib/pgsql/data/log/postgresql*.log",
			// Default log paths for CentOS / RHEL
			"/var/lib/pgsql/*/data/log/postgresql*.log",
		}
	}
	c := r.ReceiverMixin.Components(ctx, tag)
	c = append(c, r.LoggingProcessorPostgresql.Components(ctx, tag, "postgresql")...)
	return c
}

func init() {
	confgenerator.LoggingProcessorTypes.RegisterType(func() confgenerator.LoggingProcessor { return &LoggingProcessorPostgresql{} })
	confgenerator.LoggingReceiverTypes.RegisterType(func() confgenerator.LoggingReceiver { return &LoggingReceiverPostgresql{} })
}
