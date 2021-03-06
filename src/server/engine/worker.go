package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/toolkits/pkg/logger"
	"github.com/toolkits/pkg/str"

	"github.com/didi/nightingale/v5/src/models"
	"github.com/didi/nightingale/v5/src/server/common/conv"
	"github.com/didi/nightingale/v5/src/server/config"
	"github.com/didi/nightingale/v5/src/server/memsto"
	"github.com/didi/nightingale/v5/src/server/naming"
	"github.com/didi/nightingale/v5/src/server/reader"
	promstat "github.com/didi/nightingale/v5/src/server/stat"
)

func loopFilterRules(ctx context.Context) {
	// wait for samples
	time.Sleep(time.Duration(config.C.EngineDelay) * time.Second)

	duration := time.Duration(9000) * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(duration):
			filterRules()
		}
	}
}

func filterRules() {
	ids := memsto.AlertRuleCache.GetRuleIds()

	count := len(ids)
	mines := make([]int64, 0, count)

	for i := 0; i < count; i++ {
		node, err := naming.HashRing.GetNode(fmt.Sprint(ids[i]))
		if err != nil {
			logger.Warning("failed to get node from hashring:", err)
			continue
		}

		if node == config.C.Heartbeat.Endpoint {
			mines = append(mines, ids[i])
		}
	}

	Workers.Build(mines)
}

type RuleEval struct {
	rule     *models.AlertRule
	fires    map[string]*models.AlertCurEvent
	pendings map[string]*models.AlertCurEvent
	quit     chan struct{}
}

func (r RuleEval) Stop() {
	logger.Infof("rule_eval:%d stopping", r.RuleID())
	close(r.quit)
}

func (r RuleEval) RuleID() int64 {
	return r.rule.Id
}

func (r RuleEval) Start() {
	logger.Infof("rule_eval:%d started", r.RuleID())
	for {
		select {
		case <-r.quit:
			// logger.Infof("rule_eval:%d stopped", r.RuleID())
			return
		default:
			r.Work()
			interval := r.rule.PromEvalInterval
			if interval <= 0 {
				interval = 10
			}
			time.Sleep(time.Duration(interval) * time.Second)
		}
	}
}

func (r RuleEval) Work() {
	promql := strings.TrimSpace(r.rule.PromQl)
	if promql == "" {
		logger.Errorf("rule_eval:%d promql is blank", r.RuleID())
		return
	}

	value, warnings, err := reader.Reader.Client.Query(context.Background(), promql, time.Now())
	if err != nil {
		logger.Errorf("rule_eval:%d promql:%s, error:%v", r.RuleID(), promql, err)
		return
	}

	if len(warnings) > 0 {
		logger.Errorf("rule_eval:%d promql:%s, warnings:%v", r.RuleID(), promql, warnings)
		return
	}

	r.judge(conv.ConvertVectors(value))
}

type WorkersType struct {
	rules map[string]RuleEval
}

var Workers = &WorkersType{rules: make(map[string]RuleEval)}

func (ws *WorkersType) Build(rids []int64) {
	rules := make(map[string]*models.AlertRule)

	for i := 0; i < len(rids); i++ {
		rule := memsto.AlertRuleCache.Get(rids[i])
		if rule == nil {
			continue
		}

		hash := str.MD5(fmt.Sprintf("%d_%d_%s",
			rule.Id,
			rule.PromEvalInterval,
			rule.PromQl,
		))

		rules[hash] = rule
	}

	// stop old
	for hash := range Workers.rules {
		if _, has := rules[hash]; !has {
			Workers.rules[hash].Stop()
			delete(Workers.rules, hash)
		}
	}

	// start new
	for hash := range rules {
		if _, has := Workers.rules[hash]; has {
			// already exists
			continue
		}

		elst, err := models.AlertCurEventGetByRule(rules[hash].Id)
		if err != nil {
			logger.Errorf("worker_build: AlertCurEventGetByRule failed: %v", err)
			continue
		}

		firemap := make(map[string]*models.AlertCurEvent)
		for i := 0; i < len(elst); i++ {
			elst[i].DB2Mem()
			firemap[elst[i].Hash] = elst[i]
		}

		re := RuleEval{
			rule:     rules[hash],
			quit:     make(chan struct{}),
			fires:    firemap,
			pendings: make(map[string]*models.AlertCurEvent),
		}

		go re.Start()
		Workers.rules[hash] = re
	}
}

func (r RuleEval) judge(vectors []conv.Vector) {
	// ?????????rule????????????????????????????????????????????????????????????callbacks???
	// ????????????????????????????????????worker restart?????????????????????????????????????????????
	// ????????????????????????memsto.AlertRuleCache??????????????????
	curRule := memsto.AlertRuleCache.Get(r.rule.Id)
	if curRule == nil {
		return
	}

	r.rule = curRule

	count := len(vectors)
	alertingKeys := make(map[string]struct{})
	now := time.Now().Unix()
	for i := 0; i < count; i++ {
		// compute hash
		hash := str.MD5(fmt.Sprintf("%d_%s", r.rule.Id, vectors[i].Key))
		alertingKeys[hash] = struct{}{}

		// rule disabled in this time span?
		if isNoneffective(vectors[i].Timestamp, r.rule) {
			continue
		}

		// handle series tags
		tagsMap := make(map[string]string)
		for label, value := range vectors[i].Labels {
			tagsMap[string(label)] = string(value)
		}

		// handle rule tags
		for _, tag := range r.rule.AppendTagsJSON {
			arr := strings.SplitN(tag, "=", 2)
			tagsMap[arr[0]] = arr[1]
		}

		// handle target note
		targetIdent, has := vectors[i].Labels["ident"]
		targetNote := ""
		if has {
			target, exists := memsto.TargetCache.Get(string(targetIdent))
			if exists {
				targetNote = target.Note

				// ????????????ident??????????????????check??????ident??????bg???rule??????bg????????????
				// ????????????????????????????????????BG??????????????????BG??????????????????????????????????????????
				if r.rule.EnableInBG == 1 && target.GroupId != r.rule.GroupId {
					continue
				}
			}
		}

		event := &models.AlertCurEvent{
			TriggerTime: vectors[i].Timestamp,
			TagsMap:     tagsMap,
			GroupId:     r.rule.GroupId,
		}

		bg := memsto.BusiGroupCache.GetByBusiGroupId(r.rule.GroupId)
		if bg != nil {
			event.GroupName = bg.Name
		}

		// isMuted only need TriggerTime and TagsMap
		if isMuted(event) {
			logger.Infof("event_muted: rule_id=%d %s", r.rule.Id, vectors[i].Key)
			continue
		}

		tagsArr := labelMapToArr(tagsMap)
		sort.Strings(tagsArr)

		event.Cluster = r.rule.Cluster
		event.Hash = hash
		event.RuleId = r.rule.Id
		event.RuleName = r.rule.Name
		event.RuleNote = r.rule.Note
		event.Severity = r.rule.Severity
		event.PromForDuration = r.rule.PromForDuration
		event.PromQl = r.rule.PromQl
		event.PromEvalInterval = r.rule.PromEvalInterval
		event.Callbacks = r.rule.Callbacks
		event.CallbacksJSON = r.rule.CallbacksJSON
		event.RunbookUrl = r.rule.RunbookUrl
		event.NotifyRecovered = r.rule.NotifyRecovered
		event.NotifyChannels = r.rule.NotifyChannels
		event.NotifyChannelsJSON = r.rule.NotifyChannelsJSON
		event.NotifyGroups = r.rule.NotifyGroups
		event.NotifyGroupsJSON = r.rule.NotifyGroupsJSON
		event.TargetIdent = string(targetIdent)
		event.TargetNote = targetNote
		event.TriggerValue = readableValue(vectors[i].Value)
		event.TagsJSON = tagsArr
		event.Tags = strings.Join(tagsArr, ",,")
		event.IsRecovered = false
		event.LastEvalTime = now

		r.handleNewEvent(event)
	}

	// handle recovered events
	r.recoverRule(alertingKeys, now)
}

func readableValue(value float64) string {
	ret := fmt.Sprintf("%.5f", value)
	ret = strings.TrimRight(ret, "0")
	return strings.TrimRight(ret, ".")
}

func labelMapToArr(m map[string]string) []string {
	numLabels := len(m)

	labelStrings := make([]string, 0, numLabels)
	for label, value := range m {
		labelStrings = append(labelStrings, fmt.Sprintf("%s=%s", label, value))
	}

	if numLabels > 1 {
		sort.Strings(labelStrings)
	}

	return labelStrings
}

func (r RuleEval) handleNewEvent(event *models.AlertCurEvent) {
	if event.PromForDuration == 0 {
		r.fireEvent(event)
		return
	}

	_, has := r.pendings[event.Hash]
	if has {
		r.pendings[event.Hash].LastEvalTime = event.LastEvalTime
	} else {
		r.pendings[event.Hash] = event
	}

	if r.pendings[event.Hash].LastEvalTime-r.pendings[event.Hash].TriggerTime > int64(event.PromForDuration) {
		r.fireEvent(event)
	}
}

func (r RuleEval) fireEvent(event *models.AlertCurEvent) {
	if fired, has := r.fires[event.Hash]; has {
		r.fires[event.Hash].LastEvalTime = event.LastEvalTime

		if r.rule.NotifyRepeatStep == 0 {
			// ???????????????????????????????????????????????????nothing to do
			return
		}

		// ?????????????????????????????????????????????????????????????????????????????????????????????
		if event.LastEvalTime > fired.LastSentTime+int64(r.rule.NotifyRepeatStep)*60 {
			r.pushEventToQueue(event)
		}
	} else {
		r.pushEventToQueue(event)
	}
}

func (r RuleEval) recoverRule(alertingKeys map[string]struct{}, now int64) {
	for hash := range r.pendings {
		if _, has := alertingKeys[hash]; has {
			continue
		}

		delete(r.pendings, hash)
	}

	for hash, event := range r.fires {
		if _, has := alertingKeys[hash]; has {
			continue
		}

		// ??????????????????????????????????????????????????????
		if r.rule.RecoverDuration > 0 && now-event.LastEvalTime <= r.rule.RecoverDuration {
			continue
		}

		// ????????????????????????vector????????????????????????vector???????????????
		// ???????????????????????????prom??????????????????????????????????????????????????????prom???????????????????????????????????????????????????????????????
		delete(r.fires, hash)
		delete(r.pendings, hash)

		event.IsRecovered = true
		event.LastEvalTime = now
		// ????????????????????????promql???????????????????????????????????????????????????promql??????????????????????????????
		// ???????????????rule????????????????????????????????????????????????????????????
		event.RuleName = r.rule.Name
		event.RuleNote = r.rule.Note
		event.Severity = r.rule.Severity
		event.PromForDuration = r.rule.PromForDuration
		event.PromQl = r.rule.PromQl
		event.PromEvalInterval = r.rule.PromEvalInterval
		event.Callbacks = r.rule.Callbacks
		event.CallbacksJSON = r.rule.CallbacksJSON
		event.RunbookUrl = r.rule.RunbookUrl
		event.NotifyRecovered = r.rule.NotifyRecovered
		event.NotifyChannels = r.rule.NotifyChannels
		event.NotifyChannelsJSON = r.rule.NotifyChannelsJSON
		event.NotifyGroups = r.rule.NotifyGroups
		event.NotifyGroupsJSON = r.rule.NotifyGroupsJSON
		r.pushEventToQueue(event)
	}
}

func (r RuleEval) pushEventToQueue(event *models.AlertCurEvent) {
	if !event.IsRecovered {
		event.LastSentTime = event.LastEvalTime
		r.fires[event.Hash] = event
	}

	promstat.CounterAlertsTotal.WithLabelValues(config.C.ClusterName).Inc()
	logEvent(event, "push_queue")
	if !EventQueue.PushFront(event) {
		logger.Warningf("event_push_queue: queue is full")
	}
}
