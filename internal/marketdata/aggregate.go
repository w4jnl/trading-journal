package marketdata

// AggregateTo rolls bars up to a coarser timeframe by bucketing on
// floor(t/tfSeconds): open = first sub-bar, high = max, low = min, close = last,
// volume = sum. The aggregated bar keeps the OPEN time of the first sub-bar in
// its bucket, so a no-op aggregation (target == source granularity) preserves
// the original times exactly. Input must be sorted ascending by Time; output is
// ascending. The caller must ensure tf is not finer than the input (you cannot
// refine bars) - passing a finer tf simply returns each bar in its own bucket.
func AggregateTo(bars []Bar, tf Timeframe) []Bar {
	if len(bars) == 0 {
		return nil
	}
	sec := tf.Seconds()
	if sec <= 0 {
		return bars
	}
	out := make([]Bar, 0, len(bars))
	var cur Bar
	var curBucket int64 = -1
	have := false
	for _, b := range bars {
		bucket := b.Time - (b.Time % sec)
		if !have || bucket != curBucket {
			if have {
				out = append(out, cur)
			}
			cur = b // keep the first sub-bar's original Time as the open time
			curBucket = bucket
			have = true
			continue
		}
		if b.High > cur.High {
			cur.High = b.High
		}
		if b.Low < cur.Low {
			cur.Low = b.Low
		}
		cur.Close = b.Close
		cur.Volume += b.Volume
	}
	if have {
		out = append(out, cur)
	}
	return out
}
