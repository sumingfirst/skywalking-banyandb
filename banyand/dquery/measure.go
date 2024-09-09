// Licensed to Apache Software Foundation (ASF) under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Apache Software Foundation (ASF) licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package dquery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/apache/skywalking-banyandb/api/common"
	commonv1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/common/v1"
	measurev1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/measure/v1"
	"github.com/apache/skywalking-banyandb/banyand/measure"
	"github.com/apache/skywalking-banyandb/pkg/bus"
	"github.com/apache/skywalking-banyandb/pkg/logger"
	"github.com/apache/skywalking-banyandb/pkg/query"
	"github.com/apache/skywalking-banyandb/pkg/query/executor"
	logical_measure "github.com/apache/skywalking-banyandb/pkg/query/logical/measure"
)

type measureQueryProcessor struct {
	measureService measure.SchemaService
	broadcaster    bus.Broadcaster
	*queryService
}

func (p *measureQueryProcessor) Rev(message bus.Message) (resp bus.Message) {
	queryCriteria, ok := message.Data().(*measurev1.QueryRequest)
	n := time.Now()
	now := n.UnixNano()
	if !ok {
		resp = bus.NewMessage(bus.MessageID(now), common.NewError("invalid event data type"))
		return
	}
	if len(queryCriteria.Groups) > 1 {
		resp = bus.NewMessage(bus.MessageID(now), common.NewError("only support one group in the query request"))
		return
	}
	ml := p.log.Named("measure", queryCriteria.Groups[0], queryCriteria.Name)
	if e := ml.Debug(); e.Enabled() {
		e.RawJSON("req", logger.Proto(queryCriteria)).Msg("received a query event")
	}

	meta := &commonv1.Metadata{
		Name:  queryCriteria.Name,
		Group: queryCriteria.Groups[0],
	}
	ec, err := p.measureService.Measure(meta)
	if err != nil {
		resp = bus.NewMessage(bus.MessageID(now), common.NewError("fail to get execution context for measure %s: %v", meta.GetName(), err))
		return
	}

	s, err := logical_measure.BuildSchema(ec.GetSchema(), ec.GetIndexRules())
	if err != nil {
		resp = bus.NewMessage(bus.MessageID(now), common.NewError("fail to build schema for measure %s: %v", meta.GetName(), err))
		return
	}

	plan, err := logical_measure.DistributedAnalyze(queryCriteria, s)
	if err != nil {
		resp = bus.NewMessage(bus.MessageID(now), common.NewError("fail to analyze the query request for measure %s: %v", meta.GetName(), err))
		return
	}

	if e := ml.Debug(); e.Enabled() {
		e.Str("plan", plan.String()).Msg("query plan")
	}
	ctx := context.Background()
	var tracer *query.Tracer
	var span *query.Span
	if queryCriteria.Trace {
		tracer, ctx = query.NewTracer(ctx, n.Format(time.RFC3339Nano))
		span, ctx = tracer.StartSpan(ctx, "distributed-%s", p.queryService.nodeID)
		span.Tag("plan", plan.String())
		defer func() {
			data := resp.Data()
			switch d := data.(type) {
			case *measurev1.QueryResponse:
				d.Trace = tracer.ToProto()
			case common.Error:
				span.Error(errors.New(d.Msg()))
				resp = bus.NewMessage(bus.MessageID(now), &measurev1.QueryResponse{Trace: tracer.ToProto()})
			default:
				panic("unexpected data type")
			}
			span.Stop()
		}()
	}
	mIterator, err := plan.(executor.MeasureExecutable).Execute(executor.WithDistributedExecutionContext(ctx, &distributedContext{
		Broadcaster: p.broadcaster,
		timeRange:   queryCriteria.TimeRange,
	}))
	if err != nil {
		ml.Error().Err(err).Dur("latency", time.Since(n)).RawJSON("req", logger.Proto(queryCriteria)).Msg("fail to query")
		resp = bus.NewMessage(bus.MessageID(now), common.NewError("fail to execute the query plan for measure %s: %v", meta.GetName(), err))
		return
	}
	defer func() {
		if err = mIterator.Close(); err != nil {
			ml.Error().Err(err).RawJSON("req", logger.Proto(queryCriteria)).Msg("fail to close the query plan")
			if span != nil {
				span.Error(fmt.Errorf("fail to close the query plan: %w", err))
			}
		}
	}()
	result := make([]*measurev1.DataPoint, 0)
	func() {
		var r int
		if tracer != nil {
			iterSpan, _ := tracer.StartSpan(ctx, "iterator")
			defer func() {
				iterSpan.Tag("rounds", fmt.Sprintf("%d", r))
				iterSpan.Tag("size", fmt.Sprintf("%d", len(result)))
				iterSpan.Stop()
			}()
		}
		for mIterator.Next() {
			r++
			current := mIterator.Current()
			if len(current) > 0 {
				result = append(result, current[0])
			}
		}
	}()
	qr := &measurev1.QueryResponse{DataPoints: result}
	if e := ml.Debug(); e.Enabled() {
		e.RawJSON("ret", logger.Proto(qr)).Msg("got a measure")
	}
	resp = bus.NewMessage(bus.MessageID(now), qr)
	if !queryCriteria.Trace && p.slowQuery > 0 {
		latency := time.Since(n)
		if latency > p.slowQuery {
			p.log.Warn().Dur("latency", latency).RawJSON("req", logger.Proto(queryCriteria)).Int("resp_count", len(result)).Msg("measure slow query")
		}
	}
	return
}
