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
)

type MetricsReceiverTomcat struct {
	confgenerator.ConfigComponent `yaml:",inline"`

	confgenerator.MetricsReceiverSharedJVM `yaml:",inline"`

	confgenerator.MetricsReceiverSharedCollectJVM `yaml:",inline"`
}

const defaultTomcatEndpoint = "localhost:8050"

func (r MetricsReceiverTomcat) Type() string {
	return "tomcat"
}

func (r MetricsReceiverTomcat) Pipelines(_ context.Context) ([]otel.ReceiverPipeline, error) {
	targetSystem := "tomcat"

	return r.MetricsReceiverSharedJVM.
		WithDefaultEndpoint(defaultTomcatEndpoint).
		ConfigurePipelines(
			r.TargetSystemString(targetSystem),
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
	confgenerator.MetricsReceiverTypes.RegisterType(func() confgenerator.MetricsReceiver { return &MetricsReceiverTomcat{} })
}

type LoggingProcessorTomcatSystem struct {
	confgenerator.ConfigComponent `yaml:",inline"`
}

func (LoggingProcessorTomcatSystem) Type() string {
	return "tomcat_system"
}

func (p LoggingProcessorTomcatSystem) Components(ctx context.Context, tag string, uid string) []fluentbit.Component {
	c := confgenerator.LoggingProcessorParseMultilineRegex{
		LoggingProcessorParseRegexComplex: confgenerator.LoggingProcessorParseRegexComplex{
			Parsers: []confgenerator.RegexParser{
				{
					// Sample line: 11-Jan-2022 20:41:58.279 INFO [main] org.apache.catalina.startup.VersionLoggerListener.log Command line argument: -Djava.io.tmpdir=/opt/tomcat/temp
					// Sample line: 11-Jan-2022 20:41:58.283 INFO [main] org.apache.catalina.core.AprLifecycleListener.lifecycleEvent The Apache Tomcat Native library which allows using OpenSSL was not found on the java.library.path: [/usr/java/packages/lib:/usr/lib/x86_64-linux-gnu/jni:/lib/x86_64-linux-gnu:/usr/lib/x86_64-linux-gnu:/usr/lib/jni:/lib:/usr/lib]
					// Sample line: 13-Jan-2022 16:10:27.715 SEVERE [main] org.apache.catalina.core.ContainerBase.removeChild Error destroying child
					// Sample line: org.apache.catalina.LifecycleException: An invalid Lifecycle transition was attempted ([before_destroy]) for component [StandardEngine[Catalina].StandardHost[localhost].StandardContext[/examples]] in state [STARTED]
					// Sample line:         at org.apache.catalina.util.LifecycleBase.invalidTransition(LifecycleBase.java:430)
					// Sample line:         at org.apache.catalina.util.LifecycleBase.destroy(LifecycleBase.java:316)
					Regex: `^(?<time>\d{2}-[A-Z]{1}[a-z]{2}-\d{4}\s\d{2}:\d{2}:\d{2}.\d{3})\s(?<level>[A-Z]+)\s\[(?<module>[^\]]+)\]\s(?<message>(?<source>[\w\.]+)[\S\s]+)`,
					Parser: confgenerator.ParserShared{
						TimeKey: "time",
						//   13-Jan-2022 16:10:27.715
						TimeFormat: "%d-%b-%Y %H:%M:%S.%L",
						Types: map[string]string{
							"lineNumber": "integer",
						},
					},
				},
			},
		},
		Rules: []confgenerator.MultilineRule{
			{
				StateName: "start_state",
				NextState: "cont",
				Regex:     `^\d{2}-[A-Z]{1}[a-z]{2}-\d{4}\s\d{2}:\d{2}:\d{2}.\d{3}`,
			},
			{
				StateName: "cont",
				NextState: "cont",
				Regex:     `^(?!\d{2}-[A-Z]{1}[a-z]{2}-\d{4}\s\d{2}:\d{2}:\d{2}.\d{3})`,
			},
		},
	}.Components(ctx, tag, uid)

	// https://tomcat.apache.org/tomcat-10.0-doc/logging.html
	c = append(c,
		confgenerator.LoggingProcessorModifyFields{
			Fields: map[string]*confgenerator.ModifyField{
				"severity": {
					CopyFrom: "jsonPayload.level",
					MapValues: map[string]string{
						"FINEST":  "DEBUG",
						"FINER":   "DEBUG",
						"FINE":    "DEBUG",
						"INFO":    "INFO",
						"WARNING": "WARNING",
						"SEVERE":  "CRITICAL",
					},
					MapValuesExclusive: true,
				},
				InstrumentationSourceLabel: instrumentationSourceValue(p.Type()),
			},
		}.Components(ctx, tag, uid)...,
	)
	return c
}

type SystemLoggingReceiverTomcat struct {
	LoggingProcessorTomcatSystem `yaml:",inline"`
	ReceiverMixin                confgenerator.LoggingReceiverFilesMixin `yaml:",inline" validate:"structonly"`
}

func (r SystemLoggingReceiverTomcat) Components(ctx context.Context, tag string) []fluentbit.Component {
	if len(r.ReceiverMixin.IncludePaths) == 0 {
		r.ReceiverMixin.IncludePaths = []string{
			"/opt/tomcat/logs/catalina.out",
			"/var/log/tomcat*/catalina.out",
			"/var/log/tomcat*/catalina.*.log",
		}
	}
	c := r.ReceiverMixin.Components(ctx, tag)
	c = append(c, r.LoggingProcessorTomcatSystem.Components(ctx, tag, "tomcat_system")...)
	return c
}

type LoggingProcessorTomcatAccess struct {
	confgenerator.ConfigComponent `yaml:",inline"`
}

func (p LoggingProcessorTomcatAccess) Components(ctx context.Context, tag string, uid string) []fluentbit.Component {
	return genericAccessLogParser(ctx, p.Type(), tag, uid)
}

func (LoggingProcessorTomcatAccess) Type() string {
	return "tomcat_access"
}

type AccessSystemLoggingReceiverTomcat struct {
	LoggingProcessorTomcatAccess `yaml:",inline"`
	ReceiverMixin                confgenerator.LoggingReceiverFilesMixin `yaml:",inline" validate:"structonly"`
}

func (r AccessSystemLoggingReceiverTomcat) Components(ctx context.Context, tag string) []fluentbit.Component {
	if len(r.ReceiverMixin.IncludePaths) == 0 {
		r.ReceiverMixin.IncludePaths = []string{
			"/opt/tomcat/logs/localhost_access_log*.txt",
			"/var/log/tomcat*/localhost_access_log*.txt",
		}
	}
	c := r.ReceiverMixin.Components(ctx, tag)
	c = append(c, r.LoggingProcessorTomcatAccess.Components(ctx, tag, "tomcat_access")...)
	return c
}

func init() {
	confgenerator.LoggingProcessorTypes.RegisterType(func() confgenerator.LoggingProcessor { return &LoggingProcessorTomcatAccess{} })
	confgenerator.LoggingProcessorTypes.RegisterType(func() confgenerator.LoggingProcessor { return &LoggingProcessorTomcatSystem{} })
	confgenerator.LoggingReceiverTypes.RegisterType(func() confgenerator.LoggingReceiver { return &AccessSystemLoggingReceiverTomcat{} })
	confgenerator.LoggingReceiverTypes.RegisterType(func() confgenerator.LoggingReceiver { return &SystemLoggingReceiverTomcat{} })
}
