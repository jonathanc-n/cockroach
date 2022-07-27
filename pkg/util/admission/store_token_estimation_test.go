// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package admission

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/datadriven"
	"github.com/cockroachdb/pebble"
	"github.com/stretchr/testify/require"
)

func scanFloat(t *testing.T, d *datadriven.TestData, key string) float64 {
	var vstring string
	d.ScanArgs(t, key, &vstring)
	v, err := strconv.ParseFloat(vstring, 64)
	require.NoError(t, err)
	return v
}

func printLinearModel(b *strings.Builder, m tokensLinearModel) {
	fmt.Fprintf(b, "%.2fx+%d", m.multiplier, m.constant)
}

func printLinearModelFitter(b *strings.Builder, fitter tokensLinearModelFitter) {
	fmt.Fprintf(b, "int: ")
	printLinearModel(b, fitter.intLinearModel)
	fmt.Fprintf(b, " smoothed: ")
	printLinearModel(b, fitter.smoothedLinearModel)
	fmt.Fprintf(b, " per-work-accounted: %d\n", fitter.smoothedPerWorkAccountedBytes)
}

func TestTokensLinearModelFitter(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	var fitter tokensLinearModelFitter
	fitterToString := func() string {
		var b strings.Builder
		printLinearModelFitter(&b, fitter)
		return b.String()
	}
	datadriven.RunTest(t, testutils.TestDataPath(t, "tokens_linear_model_fitter"),
		func(t *testing.T, d *datadriven.TestData) string {
			switch d.Cmd {
			case "init":
				multMin := scanFloat(t, d, "mult-min")
				multMax := scanFloat(t, d, "mult-max")
				updateWithZeroActualNonZeroAccounted := false
				if d.HasArg("ingested-model") {
					d.ScanArgs(t, "ingested-model", &updateWithZeroActualNonZeroAccounted)
				}
				fitter = makeTokensLinearModelFitter(multMin, multMax, updateWithZeroActualNonZeroAccounted)
				return fitterToString()

			case "update":
				var accountedBytes, actualBytes, workCount int
				d.ScanArgs(t, "accounted-bytes", &accountedBytes)
				d.ScanArgs(t, "actual-bytes", &actualBytes)
				d.ScanArgs(t, "work-count", &workCount)
				fitter.updateModelUsingIntervalStats(
					int64(accountedBytes), int64(actualBytes), int64(workCount))
				return fitterToString()

			default:
				return fmt.Sprintf("unknown command: %s", d.Cmd)
			}
		})
}

func TestStorePerWorkTokenEstimator(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	var estimator storePerWorkTokenEstimator
	var l0Metrics pebble.LevelMetrics
	var admissionStats storeAdmissionStats

	datadriven.RunTest(t, testutils.TestDataPath(t, "store_per_work_token_estimator"),
		func(t *testing.T, d *datadriven.TestData) string {
			switch d.Cmd {
			case "init":
				estimator = makeStorePerWorkTokenEstimator()
				l0Metrics = pebble.LevelMetrics{}
				admissionStats = storeAdmissionStats{}
				return ""

			case "update":
				// The parameters are over the interval.
				var intFlushed, intIngested uint64
				d.ScanArgs(t, "flushed", &intFlushed)
				d.ScanArgs(t, "ingested", &intIngested)
				l0Metrics.BytesFlushed += intFlushed
				l0Metrics.BytesIngested += intIngested
				var admitted, writeAccounted, ingestedAccounted uint64
				d.ScanArgs(t, "admitted", &admitted)
				d.ScanArgs(t, "write-accounted", &writeAccounted)
				d.ScanArgs(t, "ingested-accounted", &ingestedAccounted)
				admissionStats.admittedCount += admitted
				admissionStats.writeAccountedBytes += writeAccounted
				admissionStats.ingestedAccountedBytes += ingestedAccounted
				if d.HasArg("bypassed-count") {
					var bypassedCount, bypassedWrite, bypassedIngested int
					d.ScanArgs(t, "bypassed-count", &bypassedCount)
					d.ScanArgs(t, "bypassed-write", &bypassedWrite)
					d.ScanArgs(t, "bypassed-ingested", &bypassedIngested)
					admissionStats.aux.bypassedCount += uint64(bypassedCount)
					admissionStats.aux.writeBypassedAccountedBytes += uint64(bypassedWrite)
					admissionStats.aux.ingestedBypassedAccountedBytes += uint64(bypassedIngested)
				}
				if d.HasArg("ignore-ingested-into-L0") {
					var ignoreIngestedIntoL0 int
					d.ScanArgs(t, "ignore-ingested-into-L0", &ignoreIngestedIntoL0)
					admissionStats.statsToIgnore.ApproxIngestedIntoL0Bytes += uint64(ignoreIngestedIntoL0)
				}
				estimator.updateEstimates(l0Metrics, admissionStats)
				wlm, ilm := estimator.getModelsAtAdmittedDone()
				require.Equal(t, wlm, estimator.atDoneWriteTokensLinearModel.smoothedLinearModel)
				require.Equal(t, ilm, estimator.atDoneIngestTokensLinearModel.smoothedLinearModel)
				var b strings.Builder
				fmt.Fprintf(&b, "interval state: %+v\n", estimator.aux)
				fmt.Fprintf(&b, "at-admission-tokens: %d\n",
					estimator.getStoreRequestEstimatesAtAdmission().writeTokens)
				fmt.Fprintf(&b, "write-tokens: ")
				printLinearModelFitter(&b, estimator.atDoneWriteTokensLinearModel)
				fmt.Fprintf(&b, "ingest-tokens: ")
				printLinearModelFitter(&b, estimator.atDoneIngestTokensLinearModel)
				return b.String()

			default:
				return fmt.Sprintf("unknown command: %s", d.Cmd)
			}
		})

}
