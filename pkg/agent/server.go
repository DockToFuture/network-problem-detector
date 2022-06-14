// SPDX-FileCopyrightText: 2022 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gardener/network-problem-detector/pkg/agent/aggregation"
	"github.com/gardener/network-problem-detector/pkg/agent/db"
	"github.com/gardener/network-problem-detector/pkg/agent/runners"
	"github.com/gardener/network-problem-detector/pkg/common"
	"github.com/gardener/network-problem-detector/pkg/common/config"
	"github.com/gardener/network-problem-detector/pkg/common/nwpd"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type jobid = string

type server struct {
	lock                 sync.Mutex
	reloadLock           sync.Mutex
	log                  logrus.FieldLogger
	agentConfigFile      string
	clusterConfigFile    string
	hostNetwork          bool
	jobs                 map[jobid]*runners.InternalJob
	revision             atomic.Int64
	currentAgentConfig   *config.AgentConfig
	currentClusterConfig *config.ClusterConfig
	obsChan              chan *nwpd.Observation
	writer               nwpd.ObservationWriter
	aggregator           aggregation.ObservationListenerExtended
	tickPeriod           time.Duration
	done                 chan struct{}

	nwpd.UnimplementedAgentServiceServer
}

func newServer(log logrus.FieldLogger, agentConfigFile, clusterConfigFile string, hostNetwork bool) (*server, error) {
	return &server{
		log:               log,
		agentConfigFile:   agentConfigFile,
		clusterConfigFile: clusterConfigFile,
		hostNetwork:       hostNetwork,
		jobs:              map[jobid]*runners.InternalJob{},
		obsChan:           make(chan *nwpd.Observation, 100),
		tickPeriod:        50 * time.Millisecond,
		done:              make(chan struct{}),
	}, nil
}

func (s *server) isHostNetwork() bool {
	return s.hostNetwork
}

func (s *server) getNetworkCfg() *config.NetworkConfig {
	networkCfg := &config.NetworkConfig{}
	if s.currentAgentConfig != nil {
		if hostNetwork && s.currentAgentConfig.HostNetwork != nil {
			networkCfg = s.currentAgentConfig.HostNetwork
		} else if !hostNetwork && s.currentAgentConfig.PodNetwork != nil {
			networkCfg = s.currentAgentConfig.PodNetwork
		}
	}
	return networkCfg
}

func (s *server) setup() error {
	cfg, err := config.LoadAgentConfig(s.agentConfigFile)
	if err != nil {
		return err
	}
	s.currentClusterConfig, err = config.LoadClusterConfig(s.clusterConfigFile)
	if err != nil {
		return err
	}

	reportPeriod := 1 * time.Minute
	timeWindow := 30 * time.Minute
	if cfg.AggregationReportPeriodSeconds != nil {
		reportPeriod = time.Duration(*cfg.AggregationReportPeriodSeconds) * time.Second
	}
	if cfg.AggregationTimeWindowSeconds != nil {
		timeWindow = time.Duration(*cfg.AggregationTimeWindowSeconds) * time.Second
	}
	s.aggregator, err = aggregation.NewObsAggregator(s.log.WithField("sub", "aggr"), reportPeriod, timeWindow,
		common.PathLogDir, s.hostNetwork)
	if err != nil {
		return err
	}

	return s.applyAgentConfig(cfg)
}

func (s *server) applyAgentConfig(cfg *config.AgentConfig) error {
	oldJobs := s.getNetworkCfg().Jobs
	if clone, err := cfg.Clone(); err != nil {
		return err
	} else {
		s.currentAgentConfig = clone
	}

	networkCfg := s.getNetworkCfg()
	if cfg.OutputDir != "" && s.writer == nil {
		prefix := "agent"
		if networkCfg.DataFilePrefix != "" {
			prefix = networkCfg.DataFilePrefix
		}
		var err error
		s.writer, err = db.NewObsWriter(s.log.WithField("sub", "writer"), cfg.OutputDir, prefix, cfg.RetentionHours)
		if err != nil {
			return err
		}
	}

	validDestHosts := map[string]struct{}{}
	applied := map[string]struct{}{}
	for _, j := range networkCfg.Jobs {
		job, err := s.parseJob(&j)
		if err != nil {
			return err
		}
		if job != nil {
			s.addOrReplaceJob(job)
			for _, s := range job.DestHosts() {
				validDestHosts[s] = struct{}{}
			}
		}
		applied[j.JobID] = struct{}{}
	}

	var obsoleteJobIDs []string
	for _, j := range oldJobs {
		if _, ok := applied[j.JobID]; !ok {
			obsoleteJobIDs = append(obsoleteJobIDs, j.JobID)
			if err := s.deleteJob(j.JobID); err != nil {
				return err
			}
		}
	}
	deleteOutdatedMetricByObsoleteJobIDs(obsoleteJobIDs)
	deleteOutdatedMetricByValidDestHosts(validDestHosts)
	if s.aggregator != nil {
		s.aggregator.UpdateValidEdges(aggregation.ValidEdges{
			JobIDs:    applied,
			SrcHosts:  validDestHosts,
			DestHosts: validDestHosts,
		})
	}
	go func() {
		// second cleanup later to deal with potential blocked requests
		// wait for request timeout
		time.Sleep(1 * time.Minute)
		deleteOutdatedMetricByObsoleteJobIDs(obsoleteJobIDs)
		deleteOutdatedMetricByValidDestHosts(validDestHosts)
	}()

	return nil
}

func (s *server) parseJob(job *config.Job) (*runners.InternalJob, error) {
	n := len(job.Args)
	if n == 0 {
		return nil, fmt.Errorf("no job args")
	}

	defaultPeriod := 1 * time.Second
	if s.getNetworkCfg().DefaultPeriod.Duration != 0 {
		defaultPeriod = s.getNetworkCfg().DefaultPeriod.Duration
	}
	rconfig := runners.RunnerConfig{
		Job:    *job,
		Period: defaultPeriod,
	}
	clusterCfg := config.ClusterConfig{}
	if s.currentClusterConfig != nil {
		clusterCfg = *s.currentClusterConfig
	}
	runner, err := runners.Parse(clusterCfg, rconfig, job.Args, true)
	if err != nil {
		return nil, fmt.Errorf("invalid job %s: %s", job.JobID, err)
	}
	if runner == nil {
		return nil, nil
	}
	return runners.NewInternalJob(runner), nil
}

func (s *server) addOrReplaceJob(job *runners.InternalJob) {
	s.lock.Lock()
	defer s.lock.Unlock()

	prefix := "starting"
	if oldJob := s.jobs[job.JobID()]; oldJob != nil {
		prefix = "restarting"
		job.SetLastRun(oldJob.GetLastRun())
	} else {
		virtualLastRun := time.Now().Add(-time.Duration(float64(job.Period()) * rand.Float64()))
		job.SetLastRun(&virtualLastRun)
	}
	s.jobs[job.JobID()] = job
	s.logStart(job, prefix)
}

func (s *server) logStart(job *runners.InternalJob, prefix string) {
	desc := job.Description()
	if desc != "" {
		desc += ", "
	}
	s.log.Infof("%s job %s: %s [%speriod=%.1fs]", prefix, job.Config().JobID, strings.Join(job.Config().Args, " "),
		desc, job.Period().Seconds())
}

func (s *server) deleteJob(jobID string) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	if oldJob := s.jobs[jobID]; oldJob != nil {
		delete(s.jobs, jobID)
		s.log.Infof("deleted job %s", jobID)
	}
	return nil
}

func (s *server) GetObservations(_ context.Context, request *nwpd.GetObservationsRequest) (*nwpd.GetObservationsResponse, error) {
	options := nwpd.ListObservationsOptions{
		Limit:           int(request.Limit),
		FilterJobIDs:    request.RestrictToJobIDs,
		FilterSrcHosts:  request.RestrictToSrcHosts,
		FilterDestHosts: request.RestrictToDestHosts,
		FailuresOnly:    request.FailuresOnly,
	}
	if request.Start != nil {
		options.Start = request.Start.AsTime()
	}
	if request.End != nil {
		options.End = request.End.AsTime()
	}
	result, err := s.writer.ListObservations(options)
	if err != nil {
		return nil, err
	}
	return &nwpd.GetObservationsResponse{
		Observations: result,
	}, nil
}

type edge struct {
	src  string
	dest string
}

func (s *server) GetAggregatedObservations(ctx context.Context, request *nwpd.GetObservationsRequest) (*nwpd.GetAggregatedObservationsResponse, error) {
	resp, err := s.GetObservations(ctx, request)
	if err != nil {
		return nil, err
	}
	result := resp.Observations
	if len(result) == 0 {
		return &nwpd.GetAggregatedObservationsResponse{}, nil
	}
	rstart := result[0].Timestamp.AsTime()
	rdelta := 1 * time.Minute
	if request.AggregationWindow != nil && request.AggregationWindow.AsDuration().Milliseconds() > 30000 {
		rdelta = request.AggregationWindow.AsDuration()
	}
	if request.Start != nil {
		rstart = request.Start.AsTime()
	}
	currEnd := rstart.Add(rdelta)
	var aggregated []*nwpd.AggregatedObservation
	currAggr := map[edge]*nwpd.AggregatedObservation{}
	addAggregations := func() {
		for _, aggr := range currAggr {
			for k, c := range aggr.JobsOkCount {
				if dur := aggr.MeanOkDuration[k]; dur != nil {
					aggr.MeanOkDuration[k] = durationpb.New(dur.AsDuration() / time.Duration(c))
				}
			}
			aggregated = append(aggregated, aggr)
		}
		currAggr = map[edge]*nwpd.AggregatedObservation{}
	}
	for _, obs := range result {
		for !obs.Timestamp.AsTime().Before(currEnd) {
			rstart = currEnd
			currEnd = rstart.Add(rdelta)
			addAggregations()
		}

		edge := edge{src: obs.SrcHost, dest: obs.DestHost}
		aggr := currAggr[edge]
		if aggr == nil {
			aggr = &nwpd.AggregatedObservation{
				SrcHost:        obs.SrcHost,
				DestHost:       obs.DestHost,
				PeriodStart:    timestamppb.New(rstart),
				PeriodEnd:      timestamppb.New(currEnd),
				JobsOkCount:    map[string]int32{},
				JobsNotOkCount: map[string]int32{},
				MeanOkDuration: map[string]*durationpb.Duration{},
			}
			currAggr[edge] = aggr
		}
		if obs.Ok {
			aggr.JobsOkCount[obs.JobID]++
			if obs.Duration != nil {
				dur := 0 * time.Second
				if d := aggr.MeanOkDuration[obs.JobID]; d != nil {
					dur = d.AsDuration()
				}
				dur += obs.Duration.AsDuration()
				aggr.MeanOkDuration[obs.JobID] = durationpb.New(dur)
			}
		} else {
			aggr.JobsNotOkCount[obs.JobID]++
		}
	}
	addAggregations()

	return &nwpd.GetAggregatedObservationsResponse{
		AggregatedObservations: aggregated,
	}, nil
}

func (s *server) stop() {
	if s.writer != nil {
		s.writer.Stop()
		s.writer = nil
	}
}

func (s *server) reloadConfig() {
	s.reloadLock.Lock()
	defer s.reloadLock.Unlock()

	agentConfig, err := config.LoadAgentConfig(s.agentConfigFile)
	if err != nil {
		s.log.Warnf("cannot load agent configuration from %s", s.agentConfigFile)
		return
	}
	clusterConfig, err := config.LoadClusterConfig(s.clusterConfigFile)
	if err != nil {
		s.log.Warnf("cannot load cluster configuration from %s", s.clusterConfigFile)
		return
	}
	changed := !reflect.DeepEqual(clusterConfig, s.currentClusterConfig) || !reflect.DeepEqual(agentConfig, s.currentAgentConfig)
	if changed {
		s.log.Infof("reloaded configuration from %s and %s", s.agentConfigFile, s.clusterConfigFile)
		s.currentClusterConfig = clusterConfig
		err = s.applyAgentConfig(agentConfig)
		if err != nil {
			s.log.Warnf("cannot apply new agent configuration from %s", s.agentConfigFile)
			return
		}
		s.log.Infof("configuration applied")
	} else {
		s.log.Debug("no reload needed")
	}
}

func (s *server) run() {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, os.Kill)

	ticker := time.NewTicker(s.tickPeriod)

	if port := s.getNetworkCfg().HttpPort; port != 0 {
		s.log.Infof("provide metrics at ':%d/metrics'", port)
		http.Handle("/metrics", promhttp.Handler())
		go func() {
			http.ListenAndServe(fmt.Sprintf(":%d", port), nil)
		}()
	}
	if s.writer != nil {
		go s.writer.Run()
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()
	if err := watcher.Add(path.Dir(s.agentConfigFile)); err != nil {
		log.Fatal(err)
	}
	if err := watcher.Add(path.Dir(s.clusterConfigFile)); err != nil {
		log.Fatal(err)
	}

	for {
		select {
		case <-s.done:
			ticker.Stop()
			s.stop()
			return
		case <-interrupt:
			ticker.Stop()
			s.stop()
			return
		case obs := <-s.obsChan:
			logObservation := s.currentAgentConfig.LogObservations
			if logObservation {
				fields := logrus.Fields{
					"src":   obs.SrcHost,
					"dest":  obs.DestHost,
					"ok":    obs.Ok,
					"jobid": obs.JobID,
					"time":  obs.Timestamp.AsTime(),
				}
				s.log.WithFields(fields).Info(obs.Result)
			}
			IncAggregatedObservation(obs.SrcHost, obs.DestHost, obs.JobID, obs.Ok)
			if obs.Ok && obs.Duration != nil {
				ReportAggregatedObservationLatency(obs.SrcHost, obs.DestHost, obs.JobID, obs.Duration.AsDuration().Seconds())
			}
			if s.writer != nil {
				s.writer.Add(obs)
			}
			if s.aggregator != nil {
				s.aggregator.Add(obs)
			}
		case err := <-watcher.Errors:
			s.log.Warning("watcher failed: %s", err)
			s.stop()
			return
		case <-watcher.Events:
			s.log.Debug("watch")
			go s.reloadConfig()
		case <-ticker.C:
			s.triggerJobs()
		}
	}
}

func (s *server) triggerJobs() {
	s.lock.Lock()
	defer s.lock.Unlock()

	for _, job := range s.jobs {
		job.Tick(s.obsChan)
	}
}
