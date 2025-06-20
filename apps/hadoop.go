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
	"fmt"

	"github.com/GoogleCloudPlatform/ops-agent/confgenerator"
	"github.com/GoogleCloudPlatform/ops-agent/confgenerator/fluentbit"
	"github.com/GoogleCloudPlatform/ops-agent/confgenerator/otel"
)

type MetricsReceiverHadoop struct {
	confgenerator.ConfigComponent                 `yaml:",inline"`
	confgenerator.MetricsReceiverSharedJVM        `yaml:",inline"`
	confgenerator.MetricsReceiverSharedCollectJVM `yaml:",inline"`
}

const defaultHadoopEndpoint = "localhost:8004"

func (r MetricsReceiverHadoop) Type() string {
	return "hadoop"
}

func (r MetricsReceiverHadoop) Pipelines(_ context.Context) ([]otel.ReceiverPipeline, error) {
	targetSystem := "hadoop"
	if r.MetricsReceiverSharedCollectJVM.ShouldCollectJVMMetrics() {
		targetSystem = fmt.Sprintf("%s,%s", targetSystem, "jvm")
	}

	return r.MetricsReceiverSharedJVM.
		WithDefaultEndpoint(defaultHadoopEndpoint).
		ConfigurePipelines(
			targetSystem,
			[]otel.Component{
				otel.NormalizeSums(),
				otel.MetricsTransform(
					otel.AddPrefix("workload.googleapis.com"),
				),
				otel.TransformationMetrics(
					otel.SetScopeName("agent.googleapis.com/"+r.Type()),
					otel.SetScopeVersion("1.0"),
				),
			},
		)
}

func init() {
	confgenerator.MetricsReceiverTypes.RegisterType(func() confgenerator.MetricsReceiver { return &MetricsReceiverHadoop{} })
}

type LoggingProcessorHadoop struct {
	confgenerator.ConfigComponent `yaml:",inline"`
}

func (LoggingProcessorHadoop) Type() string {
	return "hadoop"
}

func (p LoggingProcessorHadoop) Components(ctx context.Context, tag, uid string) []fluentbit.Component {
	// Sample log line:
	// 2022-02-01 18:09:47,136 INFO org.apache.hadoop.hdfs.server.namenode.FSEditLog: Edit logging is async:true

	regexParser := confgenerator.LoggingProcessorParseRegex{
		Regex: `(?<timestamp>\d+-\d+-\d+ \d+:\d+:\d+,\d+)\s+(?<severity>\w+)\s+(?<source>\S+):\s+(?<message>[\S\s]*)`,
		ParserShared: confgenerator.ParserShared{
			TimeKey:    "timestamp",
			TimeFormat: "%Y-%m-%d %H:%M:%S,%L",
		},
	}

	severityMappingComponents := confgenerator.LoggingProcessorModifyFields{
		Fields: map[string]*confgenerator.ModifyField{
			"severity": {
				CopyFrom: "jsonPayload.severity",
				MapValues: map[string]string{
					"TRACE":       "DEBUG",
					"DEBUG":       "DEBUG",
					"INFO":        "INFO",
					"WARN":        "WARNING",
					"DEPRECATION": "WARNING",
					"ERROR":       "ERROR",
					"CRITICAL":    "ERROR",
					"FATAL":       "FATAL",
				},
				MapValuesExclusive: true,
			},
			InstrumentationSourceLabel: instrumentationSourceValue(p.Type()),
		},
	}.Components(ctx, tag, uid)

	c := regexParser.Components(ctx, tag, uid)
	c = append(c, severityMappingComponents...)

	return c
}

type LoggingReceiverHadoop struct {
	LoggingProcessorHadoop `yaml:",inline"`
	ReceiverMixin          confgenerator.LoggingReceiverFilesMixin `yaml:",inline"`
}

func (r LoggingReceiverHadoop) Components(ctx context.Context, tag string) []fluentbit.Component {
	if len(r.ReceiverMixin.IncludePaths) == 0 {
		// Default logs for hadoop
		r.ReceiverMixin.IncludePaths = []string{
			"/opt/hadoop/logs/hadoop-*.log",
			"/opt/hadoop/logs/yarn-*.log",
		}
	}

	r.ReceiverMixin.MultilineRules = []confgenerator.MultilineRule{
		{
			StateName: "start_state",
			NextState: "cont",
			Regex:     `^\d+-\d+-\d+ \d+:\d+:\d+,\d+.*`,
		},
		{
			StateName: "cont",
			NextState: "cont",
			Regex:     `^(?!\d+-\d+-\d+ \d+:\d+:\d+,\d+).*`,
		},
	}

	c := r.ReceiverMixin.Components(ctx, tag)

	return append(c, r.LoggingProcessorHadoop.Components(ctx, tag, "hadoop")...)
}

func init() {
	confgenerator.LoggingReceiverTypes.RegisterType(func() confgenerator.LoggingReceiver { return &LoggingReceiverHadoop{} })
}
