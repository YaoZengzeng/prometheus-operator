// Copyright 2016 The prometheus-operator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/coreos/prometheus-operator/pkg/version"

	"github.com/go-kit/kit/log"
	"github.com/improbable-eng/thanos/pkg/reloader"
	"github.com/oklog/run"
	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	logFormatLogfmt                     = "logfmt"
	logFormatJson                       = "json"
	// 将STATEFULSET_ORDINAL_NUMBER设置为本pod的序列号
	statefulsetOrdinalEnvvar            = "STATEFULSET_ORDINAL_NUMBER"
	// 从环境变量中获取pod在statefulset的序号
	statefulsetOrdinalFromEnvvarDefault = "POD_NAME"
)

var (
	availableLogFormats = []string{
		logFormatLogfmt,
		logFormatJson,
	}
)

func main() {
	app := kingpin.New("prometheus-config-reloader", "")
	// 由reloader监听的config file
	cfgFile := app.Flag("config-file", "config file watched by the reloader").
		String()

	// 用环境变量替换后的配置文件的输出文件
	cfgSubstFile := app.Flag("config-envsubst-file", "output file for environment variable substituted config file").
		String()

	createStatefulsetOrdinalFrom := app.Flag(
		"statefulset-ordinal-from-envvar",
		fmt.Sprintf("parse this environment variable to create %s, containing the statefulset ordinal number", statefulsetOrdinalEnvvar)).
		Default(statefulsetOrdinalFromEnvvarDefault).String()

	logFormat := app.Flag(
		"log-format",
		fmt.Sprintf("Log format to use. Possible values: %s", strings.Join(availableLogFormats, ", "))).
		Default(logFormatLogfmt).String()

	// 用于触发Prometheus reload重载到URL
	reloadURL := app.Flag("reload-url", "reload URL to trigger Prometheus reload on").
		Default("http://127.0.0.1:9090/-/reload").URL()

	if _, err := app.Parse(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	logger := log.NewLogfmtLogger(log.NewSyncWriter(os.Stdout))
	if *logFormat == logFormatJson {
		logger = log.NewJSONLogger(log.NewSyncWriter(os.Stdout))
	}
	logger = log.With(logger, "ts", log.DefaultTimestampUTC)
	logger = log.With(logger, "caller", log.DefaultCaller)

	if createStatefulsetOrdinalFrom != nil {
		if err := createOrdinalEnvvar(*createStatefulsetOrdinalFrom); err != nil {
			logger.Log("msg", fmt.Sprintf("Failed setting %s", statefulsetOrdinalEnvvar))
		}
	}

	logger.Log("msg", fmt.Sprintf("Starting prometheus-config-reloader version '%v'.", version.Version))

	var g run.Group
	{
		ctx, cancel := context.WithCancel(context.Background())
		// ruleDirs为空
		rel := reloader.New(logger, *reloadURL, *cfgFile, *cfgSubstFile, []string{})

		g.Add(func() error {
			// 调用reloader进行watch
			return rel.Watch(ctx)
		}, func(error) {
			cancel()
		})
	}

	if err := g.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func createOrdinalEnvvar(fromName string) error {
	reg := regexp.MustCompile(`\d+$`)
	val := reg.FindString(os.Getenv(fromName))
	return os.Setenv(statefulsetOrdinalEnvvar, val)
}
