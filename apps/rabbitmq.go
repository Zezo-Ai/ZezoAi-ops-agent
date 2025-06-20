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

	"github.com/GoogleCloudPlatform/ops-agent/confgenerator"
	"github.com/GoogleCloudPlatform/ops-agent/confgenerator/fluentbit"
	"github.com/GoogleCloudPlatform/ops-agent/confgenerator/otel"
	"github.com/GoogleCloudPlatform/ops-agent/internal/secret"
)

type LoggingProcessorRabbitmq struct {
	confgenerator.ConfigComponent `yaml:",inline"`
}

func (*LoggingProcessorRabbitmq) Type() string {
	return "rabbitmq"
}

func (p *LoggingProcessorRabbitmq) Components(ctx context.Context, tag, uid string) []fluentbit.Component {
	c := confgenerator.LoggingProcessorParseRegexComplex{
		Parsers: []confgenerator.RegexParser{
			{
				// Sample log line:
				// 2022-01-31 18:01:20.441571+00:00 [erro] <0.692.0> ** Connection attempt from node 'rabbit_ctl_17@keith-testing-rabbitmq' rejected. Invalid challenge reply. **
				Regex: `^(?<timestamp>\d+-\d+-\d+\s+\d+:\d+:\d+[.,]\d+\+\d+:\d+) \[(?<severity>\w+)\] \<(?<process_id>\d+\.\d+\.\d+)\> (?<message>.*)$`,
				Parser: confgenerator.ParserShared{
					TimeKey:    "timestamp",
					TimeFormat: "%Y-%m-%d %H:%M:%S.%L%z",
				},
			},
			{
				// Sample log line:
				// 2023-02-01 12:45:14.705 [info] <0.801.0> Successfully set user tags for user 'admin' to [administrator]
				Regex: `^(?<timestamp>\d+-\d+-\d+\s+\d+:\d+:\d+[.,]\d+\d+\d+) \[(?<severity>\w+)\] \<(?<process_id>\d+\.\d+\.\d+)\> (?<message>.*)$`,
				Parser: confgenerator.ParserShared{
					TimeKey:    "timestamp",
					TimeFormat: "%Y-%m-%d %H:%M:%S.%L",
				},
			},
		},
	}.Components(ctx, tag, uid)

	// severities documented here: https://www.rabbitmq.com/logging.html#log-levels
	c = append(c,
		confgenerator.LoggingProcessorModifyFields{
			Fields: map[string]*confgenerator.ModifyField{
				"severity": {
					CopyFrom: "jsonPayload.severity",
					MapValues: map[string]string{
						"debug":   "DEBUG",
						"warning": "WARNING",
						"error":   "ERROR",
						"info":    "INFO",
						"noti":    "DEFAULT",
					},
					MapValuesExclusive: true,
				},
				InstrumentationSourceLabel: instrumentationSourceValue(p.Type()),
			},
		}.Components(ctx, tag, uid)...,
	)

	return c
}

type LoggingReceiverRabbitmq struct {
	LoggingProcessorRabbitmq `yaml:",inline"`
	ReceiverMixin            confgenerator.LoggingReceiverFilesMixin `yaml:",inline" validate:"structonly"`
}

func (r LoggingReceiverRabbitmq) Components(ctx context.Context, tag string) []fluentbit.Component {
	if len(r.ReceiverMixin.IncludePaths) == 0 {
		r.ReceiverMixin.IncludePaths = []string{
			"/var/log/rabbitmq/rabbit*.log",
		}
	}
	// Some multiline entries related to crash logs are important to capture and end in
	//
	// 2022-01-31 18:07:43.557042+00:00 [erro] <0.130.0>
	// BOOT FAILED
	// ===========
	// ERROR: could not bind to distribution port 25672, it is in use by another node: rabbit@keith-testing-rabbitmq
	//
	r.ReceiverMixin.MultilineRules = []confgenerator.MultilineRule{
		{
			StateName: "start_state",
			NextState: "cont",
			Regex:     `^\d+-\d+-\d+ \d+:\d+:\d+\.\d+\+\d+:\d+`,
		},
		{
			StateName: "cont",
			NextState: "cont",
			Regex:     `^(?!\d+-\d+-\d+ \d+:\d+:\d+\.\d+\+\d+:\d+)`,
		},
	}
	c := r.ReceiverMixin.Components(ctx, tag)
	c = append(c, r.LoggingProcessorRabbitmq.Components(ctx, tag, "rabbitmq")...)
	return c
}

func init() {
	confgenerator.LoggingReceiverTypes.RegisterType(func() confgenerator.LoggingReceiver { return &LoggingReceiverRabbitmq{} })
}

type MetricsReceiverRabbitmq struct {
	confgenerator.ConfigComponent `yaml:",inline"`

	confgenerator.MetricsReceiverShared    `yaml:",inline"`
	confgenerator.MetricsReceiverSharedTLS `yaml:",inline"`

	Password secret.String `yaml:"password" validate:"required"`
	Username string        `yaml:"username" validate:"required"`
	Endpoint string        `yaml:"endpoint" validate:"omitempty,url"`
}

const defaultRabbitmqTCPEndpoint = "http://localhost:15672"

func (r MetricsReceiverRabbitmq) Type() string {
	return "rabbitmq"
}

func (r MetricsReceiverRabbitmq) Pipelines(_ context.Context) ([]otel.ReceiverPipeline, error) {
	if r.Endpoint == "" {
		r.Endpoint = defaultRabbitmqTCPEndpoint
	}

	cfg := map[string]interface{}{
		"collection_interval": r.CollectionIntervalString(),
		"endpoint":            r.Endpoint,
		"username":            r.Username,
		"password":            r.Password.SecretValue(),
		"tls":                 r.TLSConfig(true),
	}

	return []otel.ReceiverPipeline{{
		Receiver: otel.Component{
			Type:   "rabbitmq",
			Config: cfg,
		},
		Processors: map[string][]otel.Component{"metrics": {
			otel.NormalizeSums(),
			otel.MetricsTransform(
				otel.AddPrefix("workload.googleapis.com"),
			),
			otel.TransformationMetrics(
				otel.FlattenResourceAttribute("rabbitmq.queue.name", "queue_name"),
				otel.FlattenResourceAttribute("rabbitmq.node.name", "node_name"),
				otel.FlattenResourceAttribute("rabbitmq.vhost.name", "vhost_name"),
				otel.SetScopeName("agent.googleapis.com/"+r.Type()),
				otel.SetScopeVersion("1.0"),
			),
		}},
	}}, nil
}

func init() {
	confgenerator.MetricsReceiverTypes.RegisterType(func() confgenerator.MetricsReceiver { return &MetricsReceiverRabbitmq{} })
}
