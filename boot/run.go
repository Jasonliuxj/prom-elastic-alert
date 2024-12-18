package boot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creasty/defaults"
	"github.com/dream-mo/prom-elastic-alert/conf"
	"github.com/dream-mo/prom-elastic-alert/utils/alertmanager"
	"github.com/dream-mo/prom-elastic-alert/utils/logger"
	redisx "github.com/dream-mo/prom-elastic-alert/utils/redis"
	"github.com/dream-mo/prom-elastic-alert/utils/xelastic"
	"github.com/dream-mo/prom-elastic-alert/utils/xtime"
	"github.com/go-co-op/gocron"
	"github.com/go-redis/redis/v8"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	ctx = context.Background()
)

const (
	innerSendAlertJobId = "__send_alert__"
	namespace           = "prom_elastic_alert"
	Version             = "1.0.0"
)

type ElasticAlert struct {
	appConf    *conf.AppConfig
	opts       *conf.FlagOption
	loader     Loader
	rules      sync.Map // map[string]*conf.Rule
	metrics    sync.Map // map[string]*ElasticAlertPrometheusMetrics
	schedulers sync.Map // map[string]ElasticJob
	alerts     sync.Map // map[string]AlertContent
}

type ElasticJob struct {
	Scheduler *gocron.Scheduler
	StartsAt  *time.Time
	EndsAt    *time.Time
}

func (ea *ElasticAlert) Start() {

	// Bootstrap loader
	loader := NewLoaderInstance(ea.appConf.Loader.Type)
	ea.loader = loader
	config := ea.appConf.Loader.Config
	loader.InjectConfig(config)
	logger.Logger.Infoln("Rules loading...")
	rules := ea.loader.GetRules()
	for _, rule := range rules {
		go ea.startJobScheduler(rule)
	}

	// Publish alert to redis task
	sendAlertScheduler := gocron.NewScheduler(xtime.Zone).SingletonMode()
	sendAlertScheduler.Every(ea.appConf.RunEvery.GetSeconds()).Second().Do(ea.pushAlert)
	sendAlertScheduler.StartAsync()
	job := ElasticJob{
		Scheduler: sendAlertScheduler,
	}
	ea.schedulers.Store(innerSendAlertJobId, job)
	logger.Logger.Infoln("Alert worker start...")

	// Dynamic reload job task
	loader.ReloadSchedulerJob(ea)
	logger.Logger.Infoln("Start up")

	// Consume redis alert list task
	go ea.popAlert()
}

func (ea *ElasticAlert) buildFQName(name string) string {
	return prometheus.BuildFQName(namespace, "", name)
}

func (ea *ElasticAlert) restartJobScheduler(r *conf.Rule) {
	ea.stopJobScheduler(r)
	ea.startJobScheduler(r)
}

func (ea *ElasticAlert) stopJobScheduler(r *conf.Rule) {
	j, ok := ea.schedulers.Load(r.UniqueId)
	defer func() {
		ea.rules.Delete(r.UniqueId)
		ea.schedulers.Delete(r.UniqueId)
		ea.alerts.Delete(r.UniqueId)
		ea.metrics.Delete(r.UniqueId)
	}()
	if ok {
		job := j.(ElasticJob)
		job.Scheduler.Stop()
	}
}
func (ea *ElasticAlert) Stop() {
	w := &sync.WaitGroup{}
	v := 0
	ea.schedulers.Range(func(key, value any) bool {
		v++
		return true
	})
	if v > 0 {
		w.Add(v)
		ea.schedulers.Range(func(key, value any) bool {
			job := value.(ElasticJob)
			uniqueId := key.(string)
			go func(w *sync.WaitGroup, j ElasticJob, id string) {
				logger.Logger.Infoln("Stop job: " + id)
				j.Scheduler.Stop()
				w.Done()
			}(w, job, uniqueId)
			return true
		})
		w.Wait()
	}
}

func (ea *ElasticAlert) startJobScheduler(r *conf.Rule) {
	ea.stopJobScheduler(r)
	ea.rules.Store(r.UniqueId, r)
	jobScheduler := gocron.NewScheduler(xtime.Zone).SingletonMode()
	_, _ = jobScheduler.Every(r.RunEvery.GetSeconds()).Second().Do(ea.eval, r)
	job := ElasticJob{
		Scheduler: jobScheduler,
	}
	ea.schedulers.Store(r.UniqueId, job)
	m := NewElasticAlertPrometheusMetrics()
	ea.metrics.Store(r.UniqueId, m)
	if r.Enabled {
		jobScheduler.StartAsync()
	} else {
		t := fmt.Sprintf("Rule %s is disabled", r.FilePath)
		logger.Logger.Warningln(t)
	}
}

func (ea *ElasticAlert) eval(r *conf.Rule) {
	hits := ea.runRuleQuery(r)
	f := NewRuleType(r.Query.Type)
	if f == nil {
		t := fmt.Sprintf("rule: %s query type:【%s】 is not validate!", r.FilePath, r.Query.Type)
		logger.Logger.Errorln(t)
		return
	}
	matches := f.GetMatches(r, hits)
	ea.filterMatches(r, f.FilterMatchCondition(r, matches))
}

func (ea *ElasticAlert) filterMatches(r *conf.Rule, match *Match) {
	alertVal, ok := ea.alerts.Load(r.UniqueId)
	if ok {
		alert := alertVal.(AlertContent)
		alertCopy := alert
		if match == nil {
			// Recovery alert
			endsAt := xtime.Now()
			sub := endsAt.Sub(*alertCopy.StartsAt)
			buff := time.Second * 30
			if sub > buff {
				alertCopy.EndsAt = &endsAt
			} else {
				end := alertCopy.StartsAt.Add(buff)
				alertCopy.EndsAt = &end
			}
			alertCopy.State = Resolved
		} else {
			// Update alert content
			alertCopy.Match = match
		}
		ea.alerts.Store(r.UniqueId, alertCopy)
	} else {
		// Add new alert
		if match != nil {
			alertObj := AlertContent{
				Match:    match,
				Rule:     r,
				StartsAt: &match.StartsAt,
				EndsAt:   nil,
				State:    Pending,
			}
			ea.alerts.Store(r.UniqueId, alertObj)
		}
	}
	_, ok = ea.alerts.Load(r.UniqueId)
	if match != nil {
		if ok {
			j, ok := ea.schedulers.Load(r.UniqueId)
			if ok {
				job := j.(ElasticJob)
				jobCopy := job
				jobCopy.StartsAt = job.EndsAt
				ea.schedulers.Store(r.UniqueId, jobCopy)
			}
		}
	}
}

func (ea *ElasticAlert) runRuleQuery(r *conf.Rule) []any {
	client := xelastic.NewElasticClient(r.ES, r.ES.Version)
	hits := []any{}
	size := 10000
	from := 0
	j, ok := ea.schedulers.Load(r.UniqueId)
	if j == nil || !ok {
		return hits
	}
	job := j.(ElasticJob)
	var end time.Time
	var start time.Time
	now := xtime.Now()
	if job.StartsAt == nil {
		jobCopy := job
		end = now
		start = end.Add(-ea.appConf.BufferTime.GetTimeDuration())
		jobCopy.StartsAt = &start
		jobCopy.EndsAt = &end
		ea.schedulers.Store(r.UniqueId, jobCopy)
	} else {
		jobCopy := job
		if now.Sub(*jobCopy.StartsAt) <= ea.appConf.BufferTime.GetTimeDuration() {
			jobCopy.EndsAt = &now
		} else {
			jobCopy.EndsAt = &now
			starts := now.Add(-ea.appConf.BufferTime.GetTimeDuration())
			jobCopy.StartsAt = &starts
		}
		end = *jobCopy.EndsAt
		start = *jobCopy.StartsAt
		ea.schedulers.Store(r.UniqueId, jobCopy)
	}
	dsl := r.GetQueryStringCountDSL(start, end)
	dst := &bytes.Buffer{}
	_ = json.Compact(dst, []byte(dsl))
	count, statusCode := client.CountByDSL(r.Index, dsl)
	go func() {
		ea.addQueryMetrics(r, statusCode)
	}()
	s := fmt.Sprintf("rules: %s index: %s dsl: %s hits_num: %d", r.FilePath, r.Index, dst.String(), count)
	logger.Logger.Debugln(s)
	if client != nil {
		totalPageNum := int(math.Ceil(float64(count) / float64(size)))
		maxPage := 0
		if ea.appConf.MaxScrollingCount > 0 {
			t := math.Min(float64(ea.appConf.MaxScrollingCount), float64(totalPageNum))
			maxPage = int(t)
		} else {
			maxPage = totalPageNum
		}
		w := sync.WaitGroup{}
		w.Add(maxPage)
		var lock sync.Mutex
		for p := 1; p <= maxPage; p++ {
			go func(p int, w *sync.WaitGroup) {
				from = (p - 1) * size
				dsl := r.GetQueryStringDSL(from, size, start, end)
				resultHits, _, code := client.FindByDSL(r.Index, dsl, []string{"@timestamp"})
				ea.addQueryMetrics(r, code)
				lock.Lock()
				hits = append(hits, resultHits...)
				lock.Unlock()
				w.Done()
			}(p, &w)
		}
		w.Wait()
	} else {
		t := fmt.Sprintf("%s elasticsearch client is nil", r.UniqueId)
		logger.Logger.Errorln(t)
	}
	return hits
}

func (ea *ElasticAlert) addQueryMetrics(r *conf.Rule, statusCode int) {
	f := r.GetMetricsQueryFingerprint(statusCode)
	v, _ := ea.metrics.Load(r.UniqueId)
	eam := v.(*ElasticAlertPrometheusMetrics)
	metricsVal, ok := eam.Query.Load(f)
	if ok {
		metric := metricsVal.(QueryMetrics)
		metricCopy := metric
		atomic.AddInt64(&metricCopy.Value, 1)
		eam.Query.Store(f, metricCopy)
	} else {
		eam.Query.Store(f, QueryMetrics{
			UniqueId:  r.UniqueId,
			Path:      r.FilePath,
			EsAddress: r.GetEsAddress(),
			Index:     r.Index,
			Status:    statusCode,
			Value:     1,
		})
	}
}

func (ea *ElasticAlert) addOpRedisMetrics(uniqueId string, path string, cmd string, key string, status int) {
	f := conf.GetMetricsOpRedisFingerprint(uniqueId, path, cmd, key, status)
	v, _ := ea.metrics.Load(uniqueId)
	eam := v.(*ElasticAlertPrometheusMetrics)
	metricsVal, ok := eam.OpRedis.Load(f)
	if ok {
		metric := metricsVal.(OpRedisMetrics)
		metricCopy := metric
		atomic.AddInt64(&metricCopy.Value, 1)
		eam.OpRedis.Store(f, metricCopy)
	} else {
		eam.OpRedis.Store(f, OpRedisMetrics{
			UniqueId: uniqueId,
			Path:     path,
			Cmd:      cmd,
			Key:      key,
			Status:   status,
			Value:    1,
		})
	}
}

func (ea *ElasticAlert) addWebhookNotifyMetrics(uniqueId string, path string, status int) {
	f := conf.GetMetricsWebhookNotifyFingerprint(uniqueId, path, status)
	v, _ := ea.metrics.Load(uniqueId)
	eam := v.(*ElasticAlertPrometheusMetrics)
	metricsVal, ok := eam.WebhookNotify.Load(f)
	if ok {
		metric := metricsVal.(WebhookNotifyMetrics)
		metricCopy := metric
		atomic.AddInt64(&metricCopy.Value, 1)
		eam.WebhookNotify.Store(f, metricCopy)
	} else {
		eam.WebhookNotify.Store(f, WebhookNotifyMetrics{
			UniqueId: uniqueId,
			Path:     path,
			Status:   status,
			Value:    1,
		})
	}
}

func (ea *ElasticAlert) pushAlert() {
	ea.alerts.Range(func(key, value any) bool {
		ruleUniqueId := key.(string)
		alert := value.(AlertContent)
		redisKey := alert.getUrlHashKey()
		msg := AlertSampleMessage{
			ES:    alert.Rule.ES,
			Index: alert.Rule.Index,
			Ids:   alert.Match.Ids,
		}
		bs, _ := json.Marshal(msg)
		redisx.Client.Set(ctx, redisKey, string(bs), ea.appConf.Alert.Generator.Expire.GetTimeDuration()).Result()
		url := ea.appConf.Alert.Generator.BaseUrl + "?key=" + redisKey
		message := alert.GetAlertMessage(url, msg)
		res := redisx.Client.LPush(ctx, redisx.AlertQueueListKey, message)
		if e := res.Err(); e != nil {
			go ea.addOpRedisMetrics(alert.Rule.UniqueId, alert.Rule.FilePath, "lpush", redisx.AlertQueueListKey, 0)
			t := fmt.Sprintf("pushAlert redis lpush error: %s", e.Error())
			logger.Logger.Errorln(t)
		} else {
			go ea.addOpRedisMetrics(alert.Rule.UniqueId, alert.Rule.FilePath, "lpush", redisx.AlertQueueListKey, 1)
		}
		if alert.HasResolved() {
			ea.alerts.Delete(ruleUniqueId)
		}
		return true
	})
}

func (ea *ElasticAlert) popAlert() {
	for {
		val, err := redisx.Client.BRPop(ctx, time.Second*5, redisx.AlertQueueListKey).Result()
		if err == redis.Nil {
			time.Sleep(time.Second)
			continue
		}
		if err != nil {
			t := fmt.Sprintf("BRpop %s error: %s", redisx.AlertQueueListKey, err.Error())
			logger.Logger.Infoln(t)
			time.Sleep(time.Second)
			continue
		}
		var message AlertMessage
		msg := val[1]
		e := json.Unmarshal([]byte(msg), &message)
		if e != nil {
			go ea.addOpRedisMetrics(message.UniqueId, message.Path, "brpop", redisx.AlertQueueListKey, 0)
			t := fmt.Sprintf("popAlert json.Unmarshal error: %s", e.Error())
			logger.Logger.Errorln(t)
		} else {
			go ea.addOpRedisMetrics(message.UniqueId, message.Path, "brpop", redisx.AlertQueueListKey, 1)
			now := time.Now()
			last := now.Add(-ea.appConf.AlertTimeLimit.GetTimeDuration())
			if message.StartsAt.After(now) {
				t := fmt.Sprintf("Send alert message > NOW is error, not send. %s", message.Payload)
				logger.Logger.Warningln(t)
			} else {
				if message.StartsAt.Before(now) && message.StartsAt.After(last) {
					c := ea.appConf.Alert.Alertmanager
					// Retry three times
					for i := 0; i < 3; i++ {
						res, code := alertmanager.HttpSendAlert(c.Url, c.BasicAuth.Username, c.BasicAuth.Password, message.Payload)
						go ea.addWebhookNotifyMetrics(message.UniqueId, message.Path, code)
						if res {
							break
						} else {
							t := fmt.Sprintf("Retry send to alertmanager! %d times", i+1)
							logger.Logger.Errorln(t)
						}
					}
				} else {
					t := fmt.Sprintf("last: %s startsAt:%s now:%s", last.Format(time.RFC3339), message.StartsAt.Format(time.RFC3339), now.Format(time.RFC3339))
					logger.Logger.Warningln(t)
				}
			}
		}
		time.Sleep(time.Second)
	}
}

func (ea *ElasticAlert) SetAppConf(c *conf.AppConfig) {
	ea.appConf = c
}

func NewElasticAlert(c *conf.AppConfig, opts *conf.FlagOption) *ElasticAlert {
	alert := &ElasticAlert{
		appConf:    c,
		opts:       opts,
		alerts:     sync.Map{},
		schedulers: sync.Map{},
		rules:      sync.Map{},
	}
	_ = defaults.Set(alert)
	return alert
}
