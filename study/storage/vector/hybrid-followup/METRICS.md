# Follow-up metrics

Use recall@10 over raw vector-record IDs against exact original-F32 cosine.
Report its mean and per-query minimum against both 0.99 and 0.999.

`1/Ratio@10` follows Eq.(3) of [ANN Search: Recall What Matters](https://arxiv.org/html/2606.04522#S3.SS2):

```text
10 / sum_i(returned_original_distance_i / exact_distance_i)
```

Rank both distance lists by original cosine distance, `max(0,1-cosine)`.
This is the inverse of a mean, not the mean of inverses. No secondary variant
is needed. Matching zero-distance pairs contribute 1 to the denominator;
a zero exact distance with a positive returned distance makes the score 0.
Fewer than ten returned records also scores0.

Task hit@10 collapses chunk records to unique parent symbols, as in the
read-only embed-quality evaluator. Exact task ranking considers all parents.
ANN task evaluation reports its candidate budget explicitly. Native-F32's
primary raw-record query remains K10; a separate K200 query supplies up to ten
unique task symbols. I8 rerank task hits use that configuration's full K50/100/200
reranked pool. Int8 scan task hits use its same global 128 candidate pool.
Record raw-top10 parent-hit as well when useful to distinguish the pipelines.
Metrics run outside query timers. None changes the strict recall acceptance bars.

These metrics are prepared only. No real-corpus metric pass has run in this
follow-up before the parent's heavy-work clearance.
