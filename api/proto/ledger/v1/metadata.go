package ledgerv1

// CacheHeader is the response header metadata of GetSummary that tells how
// the report was served: "HIT" from the report cache, "MISS" built and
// cached; absent when the ledger runs without a cache or the cache failed.
const CacheHeader = "x-cache"
