package request

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/gojektech/valkyrie"
	dyanmic_params "github.com/mostafatalebi/dynamic-params"
	"github.com/mostafatalebi/loadtest/pkg/assertions"
	"github.com/mostafatalebi/loadtest/pkg/config"
	"github.com/mostafatalebi/loadtest/pkg/logger"
	"github.com/mostafatalebi/loadtest/pkg/stats"
	"github.com/mostafatalebi/loadtest/pkg/stats/progress"
	variable "github.com/mostafatalebi/loadtest/pkg/variables"
	"github.com/rs/xid"
	"go.uber.org/atomic"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const DefaultStatsContainer = "default"

// Request worker is responsible to manage sending request
// with all its config to a specific endpoint
type RequestWorker struct {
	Config                 *config.Config
	SessionName            string
	StageName              string
	workerId               string
	MaxConcurrentRequests  int64
	TotalRequestsAttempted chan int8
	concurrencyMaxAchieved atomic.Int64
	Stats                  *dyanmic_params.DynamicParams
	Lock                   *sync.RWMutex
	LockConcurrencyStat    *sync.Mutex
	eventRequestAttempted  chan int8
	eventCCChanged         chan int64
	testStartTime          time.Time
	requestCounter         chan int64
	currentConcurrencyNum  atomic.Int64
	progress               *progress.ProgressIndicator
	logFileName            string
}

func NewRequestWorker(cnf *config.Config, id string) *RequestWorker {
	var sessionName = xid.New().String()
	r := &RequestWorker{
		Config:                cnf,
		StageName:             cnf.TargetName,
		workerId:			   id,
		SessionName:           sessionName,
		Stats:                 dyanmic_params.NewDynamicParams(dyanmic_params.SrcNameInternal, &sync.RWMutex{}),
		Lock:                  &sync.RWMutex{},
		LockConcurrencyStat:   &sync.Mutex{},
		eventRequestAttempted: make(chan int8),
		eventCCChanged:        make(chan int64),
		testStartTime:         time.Time{},
		requestCounter:        nil,
	}

	if cnf.EnabledLogs != true {
		logger.LogEnabled = false
	} else {
		r.Config.LogFileDirectory = logger.DefaultDirectory
		var logFileName = fmt.Sprintf("loadtest-%v-%v-%v", time.Now().Year(), time.Now().Month(), time.Now().Day()) + sessionName + ".log"
		err := logger.Initialize(logger.LogModeFile, r.Config.LogFileDirectory+logFileName)
		if err != nil {
			panic(err)
		}
		r.logFileName = logFileName
	}
	r.requestCounter = make(chan int64, r.Config.Concurrency)
	go r.CalculateMaxConcurrency()
	return r
}

func (r *RequestWorker) Do() error {
	if r.Config.Concurrency < 1 || r.Config.NumberOfRequests < 1 {
		logger.Fatal("incorrect params", "concurrent & request-count param must be greater than zero")
		return errors.New("incorrect params")
	} else if r.Config.NumberOfRequests < r.Config.Concurrency {
		logger.Fatal("incorrect params", "concurrent cannot be greater than request-count")
		return errors.New("incorrect params")
	}
	//r.publishRequestsToChannel()
	r.testStartTime = time.Now()
	wg := &sync.WaitGroup{}
	logger.InfoOut("Test Status", fmt.Sprintf("session start: %v", r.SessionName))
	logger.InfoOut("Logfile", r.logFileName)
	var bt = []byte(r.Config.FormBody)
	bd := bytes.NewBuffer(bt)
	req, err := http.NewRequest(r.Config.Method, r.Config.Url, bd)
	j := 1
	//for _ = range r.requestCounter {
	for i := int64(0); i < r.Config.NumberOfRequests; i++ {
		r.requestCounter <- int64(1)
		wg.Add(1)
		go func() {
			defer func() { <-r.requestCounter }()
			defer wg.Done()
			defer r.UpdateConcurrentReqNum(-1)
			r.eventRequestAttempted <- 1
			r.UpdateConcurrentReqNum(1)
			r.GetStat(r.workerId).IncrSuccess(0)
			if err != nil {
				logger.Error("creating request object failed", err.Error())
				return
			}
			req.Header = r.Config.Headers
			r.sendRequest(req, time.Second*time.Duration(r.Config.MaxTimeout))
		}()
		j++
	}
	wg.Wait()
	r.MergeAll()
	return nil
}

func (r *RequestWorker) DoInChain(variables variable.VariableMap, next TargetFunc) (variable.VariableMap, error) {
	defer r.UpdateConcurrentReqNum(-1)
	r.UpdateConcurrentReqNum(1)
	r.GetStat(r.workerId).IncrSuccess(0)
	if variables != nil {
		r.Config.Url = variable.ReplaceVariables(variables, r.Config.Url)
		r.Config.FormBody = variable.ReplaceVariables(variables, r.Config.FormBody)

		if r.Config.Headers != nil {
			for k, _ := range r.Config.Headers {
				hv := variable.ReplaceVariables(variables, r.Config.Headers.Get(k))
				r.Config.Headers.Set(k, hv)
			}
		}
	}
	var bt = []byte(r.Config.FormBody)
	bd := bytes.NewBuffer(bt)

	req, err := http.NewRequest(r.Config.Method, r.Config.Url, bd)
	if err != nil {
		logger.Error("creating request object failed", err.Error())
		return nil, nil
	}
	req.Header = r.Config.Headers
	variablesAnalyzed := &variable.VariableAnalysis{}
	bodyResponse, err := r.sendRequest(req, time.Second*time.Duration(r.Config.MaxTimeout))
	if r.Config.VariablesMap != nil {
		variablesAnalyzed, err = variable.NewVariableAnalysis(r.Config.VariablesMap, string(bodyResponse), "json")
		if err != nil {
			variablesAnalyzed = nil
		}
	}
	if next != nil {
		next(variables)
	}
	return variablesAnalyzed.Extract(), nil
}

func (r *RequestWorker) sendRequest(req *http.Request, tout time.Duration) ([]byte, error) {
	tn := time.Now()
	resp, err := GetHttpClient(tout).Do(req)
	if resp != nil {
		defer resp.Body.Close()
	}
	err = r.HandleResponse(r.workerId, resp, err)

	if err != nil {
		logger.Error("request failed", err.Error())
		return nil, errors.New("failed")
	} else if resp == nil {
		logger.Error("request failed", "no error and no response")
		return nil, errors.New("failed")
	}
	bodyData, err := ioutil.ReadAll(resp.Body)
	{
		// assertions on response
		if r.Config.Assertions != nil && r.Config.Assertions.Exists(assertions.AssertBodyString) {

			if err != nil {
				logger.Error("failed to read body of response", err)
				r.GetStat(r.workerId).IncrOtherErrors(1)
				return nil, errors.New("failed")
			}
			_ = r.Config.Assertions.Get(assertions.AssertBodyString).SetInput(bodyData)
		}
		_ = r.Config.Assertions.Get(assertions.AssertStatusIsOk).SetTest(resp.StatusCode)
		if err := r.Config.Assertions.ChainRunner(assertions.AssertStatusIsOk, assertions.AssertBodyString); err == nil {
			r.GetStat(r.workerId).IncrSuccess(1)
		} else {
			r.GetStat(r.workerId).IncrOtherErrors(1)
		}
	}

	var cacheUsed = int64(0)
	if r.Config.CacheUsageHeaderName != "" {
		if resp.Header.Get(r.Config.CacheUsageHeaderName) == "1" {
			cacheUsed = int64(1)
		}
	}
	dur := time.Since(tn)
	var appExecDure time.Duration
	if r.Config.ExecDurationHeaderName != "" {
		durStr := resp.Header.Get(r.Config.ExecDurationHeaderName)
		if durStr != "" {
			appExecDure, err = time.ParseDuration(durStr)
			if err != nil {
				appExecDure = 0
			}
		}
		r.GetStat(r.workerId).AddExecDuration(appExecDure)
		r.GetStat(r.workerId).AddExecShortestDuration(appExecDure)
		r.GetStat(r.workerId).AddExecLongestDuration(appExecDure)
	}
	r.GetStat(r.workerId).IncrCacheUsed(cacheUsed)
	r.GetStat(r.workerId).AddMainDuration(dur)
	r.GetStat(r.workerId).AddLongestDuration(dur)
	r.GetStat(r.workerId).AddShortestDuration(dur)
	return bodyData, nil
}

func (r *RequestWorker) HandleResponse(profileName string, resp *http.Response, err interface{}) error {
	if err != nil || resp == nil {
		if ve, ok := err.(net.Error); ok && ve.Timeout() {
			r.GetStat(profileName).IncrTimeout(1)
			logger.Error("request timeout", "["+profileName+"]"+ve.Error())
		} else if ve, ok := err.(net.Error); ok && !ve.Timeout() {
			r.GetStat(profileName).IncrOtherErrors(1)
			logger.Error("request timeout", "["+profileName+"]"+ve.Error())
		} else if ve, ok := err.(*valkyrie.MultiError); ok {
			errStr := ve.Error()
			if err := ve.HasError(); strings.Contains(errStr, "context deadline exceeded") {
				r.GetStat(profileName).IncrTimeout(1)
				r.GetStat(profileName).IncrTotalSent(1)
				return errors.New("context timeout => [" + profileName + "]" + err.Error())
			} else if err := ve.HasError(); strings.Contains(err.Error(), "connect: connection refused") {
				r.GetStat(profileName).IncrConnRefused(1)
				r.GetStat(profileName).IncrTotalSent(1)
				return errors.New("connection refused => [" + profileName + "]" + err.Error())
			} else {
				r.GetStat(profileName).IncrOtherErrors(1)
				r.GetStat(profileName).IncrTotalSent(1)
				return errors.New("other errors => [" + profileName + "]" + err.Error())
			}
		} else {
			errStr := ""
			if v, ok := err.(error); ok {
				errStr = v.Error()
			}
			r.GetStat(profileName).IncrFailed(500, 1)
			r.GetStat(profileName).IncrTotalSent(1)
			return errors.New("other errors => [" + profileName + "]" + errStr)
		}
	} else if resp.StatusCode == 504 {
		r.GetStat(profileName).IncrTimeout(1)
		return errors.New("server timeout => [" + profileName + "]")
	}
	r.GetStat(profileName).IncrTotalSent(1)
	return nil
}

func (r *RequestWorker) AddStat(name string, s *stats.StatsCollector) {
	r.Stats.Add(name, s)
}

func (r *RequestWorker) GetStat(name string) *stats.StatsCollector {
	r.Lock.Lock()
	defer r.Lock.Unlock()
	s := r.Stats.Get(name)
	if s == nil {
		return nil
	}
	if v, ok := s.(*stats.StatsCollector); ok {
		return v
	}
	return nil
}

// the value is a signed +1 or -1, and the worker on the other end
// uses this value to calculate max concurrency achieved
func (r *RequestWorker) UpdateConcurrentReqNum(val int8) {
	r.LockConcurrencyStat.Lock()
	defer r.LockConcurrencyStat.Unlock()
	if val != -1 && val != 1 {
		return
	} else if val == 1 {
		r.currentConcurrencyNum.Add(int64(val))
	} else if val == -1 {
		r.currentConcurrencyNum.Add(int64(val))
	}
	r.eventCCChanged <- r.currentConcurrencyNum.Load()
}

func (r *RequestWorker) CalculateMaxConcurrency() {
	for sig := range r.eventCCChanged {
		r.GetStat(r.workerId).UpdateMaxConcurrencyAchieved(sig)
	}
}
func (r *RequestWorker) MergeAll() stats.StatsCollector {
	var totalStats stats.StatsCollector

	r.Stats.Iterate(func(key string, value interface{}) {
		v, ok := value.(*stats.StatsCollector)
		if !ok {
			return
		}
		newStats := v.Merge(&totalStats)
		newStats.Key = "total"
		totalStats = newStats
	})
	totalStats.CalculateAverage()
	totalStats.CalculateExecAverageDuration()
	return totalStats
}
