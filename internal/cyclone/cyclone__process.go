/*-
 * Copyright © 2016-2017, Jörg Pernfuß <code.jpe@gmail.com>
 * Copyright © 2016, 1&1 Internet SE
 * All rights reserved.
 *
 * Use of this source code is governed by a 2-clause BSD license
 * that can be found in the LICENSE file.
 */

package cyclone // import "github.com/mjolnir42/cyclone/internal/cyclone"

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/mjolnir42/erebos"
	"github.com/mjolnir42/eyewall"
	"github.com/mjolnir42/legacy"
	metrics "github.com/rcrowley/go-metrics"
)

// process evaluates a metric and raises alarms as required
func (c *Cyclone) process(msg *erebos.Transport) error {
	if msg == nil || msg.Value == nil {
		logrus.Warnf("Ignoring empty message from: %d", msg.HostID)
		if msg != nil {
			c.delay.Use()
			go func() {
				c.commit(msg)
				c.delay.Done()
			}()
		}
		return nil
	}

	m := &legacy.MetricSplit{}
	if err := json.Unmarshal(msg.Value, m); err != nil {
		logrus.Errorf("Invalid data: %s", err.Error())
		return err
	}

	// ignore metrics configured to discard
	if c.discard[m.Path] {
		metrics.GetOrRegisterMeter(`/metrics/discarded.per.second`,
			*c.Metrics).Mark(1)
		// mark as processed
		c.delay.Use()
		go func() {
			msg.Commit <- &erebos.Commit{
				Topic:     msg.Topic,
				Partition: msg.Partition,
				Offset:    msg.Offset,
			}
			c.delay.Done()
		}()
		return nil
	}

	// handle heartbeats
	switch m.Path {
	case `_internal.cyclone.heartbeat`:
		c.heartbeat()
		return nil
	}

	// non-heartbeat metrics count towards processed metrics
	metrics.GetOrRegisterMeter(`/metrics/processed.per.second`,
		*c.Metrics).Mark(1)

	// metric has no tags for matching with configuration profiles
	if len(m.Tags) == 0 {
		logrus.Debugf(
			"[%d]: skipping metric %s with no tags from %d",
			c.Num, m.Path, m.AssetID,
		)
		return nil
	}

	// fetch configuration profile information
	thr, err := c.lookup.LookupThreshold(m.LookupID())
	if err == eyewall.ErrUnconfigured {
		logrus.Debugf(
			"Cyclone[%d], No thresholds configured for %s from %d",
			c.Num, m.Path, m.AssetID,
		)
		return nil
	} else if err != nil {
		logrus.Errorf(
			"Cyclone[%d], ERROR fetching threshold data."+
				" Lookup service available?",
			c.Num,
		)
		return err
	}

	var evaluations int64
	evals := metrics.GetOrRegisterMeter(
		`/evaluations.per.second`,
		*c.Metrics,
	)
	logrus.Debugf(
		"Cyclone[%d], Forwarding %s from %d for evaluation (%s)",
		c.Num, m.Path, m.AssetID, m.LookupID(),
	)

thrloop:
	for key := range thr {
		evalThreshold := false

		// check if this threshold's ID is found in the metric's tags
	tagloop:
		for _, t := range m.Tags {
			if !isUUID(t) {
				continue tagloop
			}
			if thr[key].ID == t {
				evalThreshold = true
				break tagloop
			}

		}

		// metric is not evaluated against this threshold
		if !evalThreshold {
			continue thrloop
		}

		// perform threshold evaluation
		alarmLevel, value, ev := c.evaluate(m, thr[key])
		evaluations += ev

		// construct alarm
		al := AlarmEvent{
			Source: fmt.Sprintf("%s / %s",
				thr[key].MetaTargethost,
				thr[key].MetaSource,
			),
			EventID:    thr[key].ID,
			Version:    c.Config.Cyclone.APIVersion,
			Sourcehost: thr[key].MetaTargethost,
			Oncall:     thr[key].Oncall,
			Targethost: thr[key].MetaTargethost,
			Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
			Check:      fmt.Sprintf("cyclone(%s)", m.Path),
			Monitoring: thr[key].MetaMonitoring,
			Team:       thr[key].MetaTeam,
		}
		al.Level, _ = strconv.ParseInt(alarmLevel, 10, 64)
		switch al.Level {
		case 0:
			al.Message = `Ok.`
		default:
			al.Message = fmt.Sprintf(
				"Metric %s has broken threshold. Value %s %s %d",
				m.Path,
				value,
				thr[key].Predicate,
				thr[key].Thresholds[alarmLevel],
			)
		}
		if al.Oncall == `` {
			al.Oncall = `No oncall information available`
		}
		c.updateEval(thr[key].ID)
		if c.Config.Cyclone.TestMode {
			// do not send out alarms in testmode
			continue thrloop
		}
		alrms := metrics.GetOrRegisterMeter(`/alarms.per.second`,
			*c.Metrics)
		alrms.Mark(1)
		c.delay.Use()
		go c.sendAlarm(al)
	}
	evals.Mark(evaluations)
	if evaluations == 0 {
		logrus.Debugf(
			"Cyclone[%d], metric %s(%d) matched no configurations",
			c.Num, m.Path, m.AssetID,
		)
	}
	return nil
}

// isUUID validates if a string is one very narrow formatting of a
// UUID. Other valid formats with braces etc are not accepted.
func isUUID(s string) bool {
	const reUUID string = `^[[:xdigit:]]{8}-[[:xdigit:]]{4}-[1-5][[:xdigit:]]{3}-[[:xdigit:]]{4}-[[:xdigit:]]{12}$`
	const reUNIL string = `^0{8}-0{4}-0{4}-0{4}-0{12}$`
	re := regexp.MustCompile(fmt.Sprintf("%s|%s", reUUID, reUNIL))

	return re.MatchString(s)
}

// vim: ts=4 sw=4 sts=4 noet fenc=utf-8 ffs=unix
