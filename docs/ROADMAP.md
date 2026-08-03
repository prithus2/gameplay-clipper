# Gameplay Clipper — Next Steps Analysis

_Written 2026-07-30. Platform facts verified against Meta / Google docs on that date; re-check before implementing, Meta's Instagram surface moves fast._

---

## 1. Where the pipeline stands

What exists today (and it's a real pipeline, not a toy):

```
recording → proxy (720p) → detectors → deterministic scoring → candidate windows
          → SQLite queue (pending_review) → local review UI → [human approves]
          → Gemini metadata (title/thumb frame/crop focus) → cut → 9:16 → thumb
```

The good architectural decisions already made, which everything below leans on:

- **Detection is deterministic and separated from judgment.** `pkg/scoring` decides *which* window; the LLM only judges and titles. That's the right split, and it's why the LLM evidence prompt works.
- **Idempotency is keyed properly.** `store.RunJob(session, stage, config_hash)` and `FindOrInsertCandidate` mean re-running after a profile tweak recomputes only what changed. This is the single most valuable thing you built, because every idea below involves re-running the pipeline a lot.
- **Human gate before render, not after.** Candidates are `pending_review` by default. Keep this invariant — the publish work below must not quietly bypass it.
- **Per-game profiles as data, not code.** `profiles/*.yaml` with signals + archetypes + weights.

### The strategic problem

**The pipeline is open-loop.** Every quality decision in it — which signals matter, what threshold, how long a clip should be, what the title says, where to crop — is currently a *guess that never gets graded*. Profile weights were hand-tuned against footage. `gow.yaml` even documents that letterbox detection was tried and failed on real video: that's the right instinct, but it was discovered by hand, once, and written into a comment.

You cannot tune your way to reach in the dark. So the ordering below is not "uploader first because uploading is annoying" — it's **publish + measure first, because measurement is the prerequisite for every other improvement being verifiable instead of vibes.**

Your instinct to put auto-upload at #1 is correct. But build it as *publish **and** measure*, not just publish. The `media_id` that comes back from Instagram is the join key for the whole rest of this roadmap.

---

## 2. Constraint reality check

You said: **Creator account, no Facebook Page linked, Instagram only.** That specific combination is the tightest of the API tiers, and it blocks three things you'll want.

| Capability | Instagram Login (your setup) | + Facebook Page linked |
|---|---|---|
| Publish Reels via API | ✅ but **only from a public `video_url`** | ✅ **plus resumable upload straight from disk** |
| Own post insights (views, reach, saves, shares, watch time) | ✅ | ✅ |
| `business_discovery` (competitor public data) | ❌ **not available** | ✅ |
| Audio search / attach `audio_id` to a Reel | ❌ **not available** | ✅ (cleared tracks only) |
| Trial Reels (publish to non-followers) | ✅ | ✅ |

### Recommendation 0 — link a Facebook Page. Do this before writing any code.

It is an afternoon of Meta app setup (create app, Facebook Login for Business, connect Page, complete Page Publishing Authorization, request `instagram_basic` / `instagram_content_publish` / `instagram_manage_insights`), and it unlocks:

1. **Upload from local disk** via `rupload.facebook.com` resumable sessions. Without it you must put every `short.mp4` on a public HTTPS URL for Meta to cURL — meaning an S3/R2 bucket or a Cloudflare tunnel to a local server, per clip, with the associated "my gameplay clips are briefly public on the internet" wrinkle. This one difference alone probably halves the Phase 1 build.
2. **`business_discovery`** — the only sanctioned way to read competitor accounts. Without it, your idea #2 has no legal Instagram data source at all.
3. **Audio attachment** — the closest thing to your "trendy audio" signal that exists in an API.

Everything below assumes you've done this. If you decide not to, note it in Phase 1 and budget for the hosting shim.

### Hard limits that affect the renderer today

- **Reels published via API are capped at 90s** (some accounts still 60s), ≤300 MB, 9:16, H.264/HEVC. In-app upload allows far longer.
  → **`profiles/gow.yaml` sets `max_length_s: 180` for the cutscene archetype.** Any approved GoW cutscene over 90s is currently unpublishable through the API. Needs either a publish-time length constraint, auto-split into Part 1/Part 2, or a "route to manual in-app upload" flag.
- **100 API-published posts per rolling 24h** (query `GET /{ig-id}/content_publishing_limit`). Not a real constraint for you, but track it rather than discovering it.
- **Containers expire in 24h**; poll `?fields=status_code` roughly once a minute for up to ~5 minutes before treating a publish as failed.
- **Browser automation for uploading: don't.** Publishing through the official Graph API is explicitly supported; simulating logins or driving the web UI is the thing Meta's detection has been specifically tightened against, and it puts the account you're trying to grow at risk. Reserve Chrome/manual for the one case the API genuinely can't do (attaching a big trending song), and treat that as a human-in-the-loop queue, not automation.

---

## 3. Phase 1 — the publish + measure spine

**Goal:** approving a clip in the review UI results in a published Reel, a local copy for cross-posting, and performance numbers flowing back into SQLite.

### 3.1 New package: `pkg/publish`

```go
type Target interface {
    Name() string                                  // "instagram"
    Publish(ctx, RenderedClip, PostCopy) (string, error) // returns platform media id
    Insights(ctx, mediaID string) (Metrics, error)
}
```

Instagram flow: create container (`media_type=REELS`, `cover_url`, `share_to_feed=true`, `caption`) → resumable upload from disk → poll `status_code` until `FINISHED` → `media_publish` → store `media_id`.

Make it an interface from day one even though Instagram is the only target. Phase 3 and the YouTube argument in §6 both plug in here, and retrofitting an interface after the fact is the kind of work that never gets done.

### 3.2 Schema additions

```sql
CREATE TABLE publication (
  id INTEGER PRIMARY KEY,
  candidate_id INTEGER NOT NULL,
  platform TEXT NOT NULL,             -- instagram | youtube | ...
  variant_id INTEGER,                 -- Phase 3
  platform_media_id TEXT,
  caption TEXT, hashtags TEXT, hook_text TEXT,
  is_trial INTEGER DEFAULT 0,         -- Phase 3
  status TEXT NOT NULL,               -- queued|uploading|published|failed|graduated
  error TEXT,
  published_at TEXT, created_at TEXT, updated_at TEXT
);

CREATE TABLE metric_snapshot (
  id INTEGER PRIMARY KEY,
  publication_id INTEGER NOT NULL,
  captured_at TEXT NOT NULL,
  age_hours REAL NOT NULL,            -- so 1h/24h/7d rows are comparable across posts
  views INTEGER, reach INTEGER, likes INTEGER, comments INTEGER,
  saves INTEGER, shares INTEGER,
  total_watch_time_ms INTEGER, avg_watch_time_ms INTEGER,
  raw_json TEXT
);
```

**Snapshot at fixed ages (+1h, +24h, +7d), never just "latest".** Metrics are cumulative; comparing a 3-day-old post to a 3-hour-old post is the classic way to learn nothing. `age_hours` is what makes any later analysis honest.

The metrics that matter, in order, per Instagram's own 2026 ranking signals: **average watch time, shares ÷ reach, saves, then likes.** Store views for sanity but don't optimize it — it's the most gameable and least predictive of distribution.

### 3.3 Render-time publish compliance

Add a validation pass before publish: duration ≤90s, resolution exactly 1080×1920, file ≤300 MB, `+faststart`, audio present, and a real 1080×1920 cover image. Fail loudly at render time, not at the API.

### 3.4 Review UI changes

The current server (`review.RunUntilDone`) is per-session and blocks the CLI until you click Finish. That's fine for reviewing, wrong for publishing. Split it:

- Keep the blocking session reviewer as-is.
- Add `clipper serve` — a **persistent library UI across all sessions**, showing every clip and its state: `pending_review → approved → rendered → queued → published → measured`. Reject/approve, edit caption, schedule, retry failed publishes, and see the metrics table sorted by avg watch time.

Approve gets two buttons: **Approve & publish** and **Approve (local only)**. Local-only still lands in `dist/` for hand-uploading elsewhere — that's your stated requirement and it stays first-class.

### 3.5 Copy generation

At approval time you already call Gemini. Extend `ClipMetadata` with `hook_text` (≤5 words, for the on-screen overlay), `caption`, and `hashtags`, generated from a **named template** whose id gets stored on the publication row. Templates you can then rank by performance. Don't free-form every caption — you can't learn from n=1 of everything.

---

## 4. Phase 2 — the retention render pass

This is where per-clip quality actually jumps, it's pure ffmpeg work you already know, and **most of the inputs are already being computed and thrown away.**

### 4.1 `crop_focus` is generated and discarded — fix this first

`llm.ClipMetadata.CropFocus{X,Y}` is produced by Gemini, persisted to `metadata.json`, and then **never read.** `cutter.reformatTo916` is a hard-coded centre blur-pad:

```
scale/crop 1080x1920 + boxblur → background; scale=1080:-2 → foreground centred
```

So on a 16:9 source you're shipping a 1080-wide letterboxed strip in the middle of a blurred pillar — roughly 40% of the frame is decorative blur, on the platform where screen real estate is retention. The model is already telling you where the action is. Two modes worth having:

- **Tight crop** (`crop=1080:1920` with `x` driven by `crop_focus.x`) for action where the subject is localised — the default for kills/fatalities.
- **Blur-pad** (current behaviour) for wide/cinematic frames where cropping destroys the composition — cutscenes.

Let the LLM pick the mode; it's already looking at the frames. Ship a side-by-side of both for a few clips before committing to a default.

### 4.2 Burn in captions — the transcript already exists

`pkg/signal/asr.go` runs whisper.cpp, gets back timestamped `asrSegment`s, extracts keyword hits, and **discards the transcript.** That's a free, already-paid-for caption track.

Burned-in captions are one of the strongest retention levers on Reels (most feed viewing is sound-off, and IG's own auto-captions are neither styled nor reliably enabled). Render `.ass` subtitles from the segments and burn them with the `subtitles` filter — styled, positioned in the safe zone (avoid the bottom ~250px where IG overlays UI), 2–4 words at a time rather than full sentences.

### 4.3 Hook the first 1.5 seconds

Currently a clip starts at `start - lead_pad_s` — i.e. deliberately *before* the moment, in the calm. That's correct for comprehension and wrong for retention. Three cheap interventions, in ROI order:

1. **Overlay hook text** for the first ~2s (`drawtext` from `hook_text`, top third, safe zone). Your idea #2.1, and you don't need competitor data to start — this is a known-good default.
2. **Cold open**: prepend ~0.4s of the payoff frame (the thumbnail frame you already choose) before the build-up. Costs one `-loop 1` input and a concat.
3. **Loopability**: if the last frame is close to the first, a seamless loop turns one view into replays, and replays are counted. Cheap heuristic — compare frame hashes at both ends and prefer trims that loop.

### 4.4 Audio: two things, both currently missing

- **Loudness normalise.** `reformatTo916` uses `-c:a copy`, so output loudness is whatever the capture was — and gameplay audio is extremely peaky. Add `loudnorm=I=-14:TP=-1.5:LRA=11` (two-pass for accuracy). Quiet Reels get skipped.
- **Music bed, legally.** Since the API can't attach big trending tracks, build a small local royalty-free library with a beat grid, duck game audio under it (`sidechaincompress`), and where possible align the cut to land the hit (kill/fatality) on a beat. This is the ToS-safe substitute for "trendy audio", and beat-aligned cuts are a large part of why creator clips *feel* professional.

### 4.5 Render bug/quality list (small, worth doing while you're in there)

- **Double encode.** `cutOriginal` encodes clip.mp4 with x264 medium, then `reformatTo916` re-encodes *that* into short.mp4. Two generation losses and ~2× the encode time. Emit both outputs from the original in one ffmpeg invocation using `split`.
- **`generateThumbnail` will break on real titles.** The filter string interpolates the title into `drawtext=text='...'` escaping only `'`. Gemini titles contain colons (`"Baraka's Fatality: ..."`) and `:` is the ffmpeg filter option separator — that's a hard failure. Use `textfile=` with the title written to a temp file. Also specify `fontfile=` explicitly rather than relying on a default font existing.
- **Cover is the wrong shape.** `thumb.png` is a 16:9 frame from the original with text at `y=h-220`. Reels covers are 1080×1920 and get cropped to 1:1 in the profile grid. Generate the cover from the 9:16 render with the crop grid in mind.
- **`-movflags +faststart`** on anything you upload.
- **`sessionIDFor` hashes the absolute path**, so moving or renaming a recording orphans its whole session and re-runs everything. Hash the first few MB of content instead.

---

## 5. Phase 3 — Trial Reels as an A/B harness

**This is the piece I'd most want you to notice, because it changes the pipeline from a renderer into an experiment machine, and it's officially supported.**

Meta's content publishing API supports **Trial Reels**: `trial_params.graduation_strategy` on a `media_type=REELS` container. A trial reel is shown **only to non-followers**, and can be graduated to your followers either `MANUAL`ly or automatically if it performs (`SS_PERFORMANCE`).

Read that again in the context of what you're trying to learn. It is a free, sanctioned, zero-risk-to-your-feed test harness *on your own account* against a cold audience — which is exactly the audience that determines reach. You don't have to infer what works from competitors. You can measure it.

**The harness:**

1. **Variant renderer.** One approved candidate → N renders differing in exactly one axis: hook text A vs B, tight crop vs blur-pad, captions on/off, 20s vs 45s trim, music vs raw audio. Store each as a `variant` row with its parameters.
2. Publish variants as **trial reels**, spaced out, one axis at a time.
3. Snapshot at +24h. Compare **avg watch time** and **shares/reach**, not views.
4. Graduate the winner to followers. Record the ordinal result: "hook style X beat hook style Y on this archetype."
5. Winning parameters become the render defaults per `(game, archetype)`.

**Be honest about the statistics.** You will not have the volume for regression modelling — a solo creator produces tens of posts a month, not thousands. So design the loop around **ordinal comparisons within a matched pair** (same clip, one variable changed, same week) rather than fitting a model on absolute numbers. Pairwise wins accumulate into real knowledge far faster than a dataset of 40 posts with 20 confounded variables. Resist the temptation to build a scoring model until you have ~200 paired results.

This phase is also what makes Phase 4 safe: competitor analysis generates *hypotheses*, and this is the thing that tests them.

---

## 6. Phase 4 — competitor / format intelligence (your idea #2, done properly)

Your instinct is right but the framing needs one correction, and it's the most important thing in this document:

> **You cannot measure competitors' reach on Instagram, and the metrics you *can* get are the ones Instagram cares about least.**

`business_discovery` returns, for a public professional account: `followers_count`, `media_count`, `biography`, and per-media `caption`, `media_type`, `media_url`, `permalink`, `timestamp`, `like_count`, `comments_count`. **No views, no reach, no saves, no shares, no watch time** — those are private to the account owner. Meanwhile IG's 2026 ranking leans on watch time and shares-per-reach, and *de*-emphasises likes. So a like-count-driven competitor model optimises for the weakest available proxy.

Scraping to get around this is not the answer: it violates Meta's terms, risks the account you're building, and the data is unstable anyway.

So split the problem in two, and use a different source for each half.

### 6a. Learn **form** from Instagram (what you can legitimately see)

For 5–10 chosen creators, poll `business_discovery` on a schedule and extract structural features from what's publicly visible:

- caption structure: length, question-opener vs statement, hashtag count and mix (broad vs niche), CTA presence
- posting cadence and time-of-day
- format mix: reel vs carousel vs image, series/"Part N" usage
- from the media thumbnail: **does the first frame carry text?** how much, where, how large? (this is directly your idea #2.1, and it is observable at scale)
- title/hook vocabulary — the phrasings that recur across accounts

Normalise engagement as `(likes + comments) / followers_count` so a 2M-follower account doesn't dominate. Treat the result as **weak evidence about form**, never as a reach model.

### 6b. Learn **performance** from YouTube (where view counts are public and free)

Most gaming clip accounts cross-post the same clips to YouTube Shorts. The **YouTube Data API v3 is free** and returns real `viewCount`, `likeCount`, `commentCount`, `duration`, `title`, `thumbnail`, and publish time for any public channel.

The quota trick that makes this practically free: **never use `search.list`** (100 units/call, ~100 calls/day). Instead resolve each channel's uploads playlist once, then page `playlistItems.list` (1 unit) and batch `videos.list` with up to 50 ids per call (1 unit). Ten creators × 200 recent videos ≈ **50 units of your 10,000/day budget.** You can refresh daily and keep a real longitudinal dataset.

Then the analysis that actually matters — and the part most people get wrong:

> **Normalise per channel and per video age before comparing anything.** Compute each video's views as a ratio to that channel's trailing median, and only compare videos of similar age. Skip this and you will confidently "discover" that big channels get more views.

With that in place you can ask real questions: does a text hook in the first frame correlate with outperformance? Does 20–30s beat 45–60s for kill clips? Do "how to beat X" tutorial framings outperform raw highlights (your #2.3 hypothesis — and note the answer likely differs by game and by whether the boss is currently hard/topical)?

### 6c. The output must be a human-readable brief, not a black box

End of pipeline: a generated `docs/signals/YYYY-MM-DD.md` — ranked observations, each with sample size, effect size, and a proposed concrete change ("cut the `mk` fatality archetype `max_length_s` from 45 to 30", "add hook template H4"). **You approve it; then it edits the profile/render defaults.** Never let a scraper silently rewrite `profiles/*.yaml`. Then verify the change with a Phase 3 trial-reel pair.

### 6d. On trending audio, specifically

There's no honest API for "what's trending right now" without third-party scrapers. Realistic plan:

- With a Page linked, use the audio search endpoint for **cleared** tracks and attach `audio_id` for the routine cases.
- Keep a **manual audio queue** in the library UI: the top ~20% of clips by predicted quality get flagged "hold for in-app audio", you upload those by hand from the phone with a genuinely trending song, and you record which sound you used so it can be compared against the API-published cohort. Manual on 3 clips a week is fine; manual on 30 is what you built this pipeline to escape.

### 6e. One more argument for cross-posting

You said Instagram only. Consider posting to YouTube Shorts anyway — **not for the YouTube reach, for the telemetry.** The YouTube Analytics API gives you **audience retention curves** on your own videos: the exact second viewers drop off in a 30s clip. Instagram will never tell you that. Fix your hooks and pacing using YouTube's curve, ship the improved version to Instagram. That is a better return on a duplicate upload than the Shorts views themselves.

---

## 7. Phase 5 — eval harness and profile bootstrapping

Less glamorous, and the reason the earlier phases stay safe to change.

### 7.1 Golden-set evaluation

You already have `testdata/valorant/golden/` with `expected_events.json`. Extend it into a proper eval: hand-labelled ground-truth highlight windows for 2–3 recordings per game, and a `clipper eval` command reporting **precision / recall / mean IoU** of detected candidate windows against the labels. Right now, changing a weight in `gow.yaml` is an unmeasured act of faith. With this, profile tuning becomes an optimisation problem you can actually run — including a simple grid/random search over weights and thresholds.

### 7.2 Mine your own rejections

`candidate.reject_reason` is already captured and never used. Every review session generates labelled negatives for free. After ~100 reviews you can ask which signals dominate rejected candidates and cut their weight — cheaper and more honest than any competitor analysis, because it's your taste on your games.

### 7.3 Profile authoring assistant

Onboarding a new game currently means hand-tuning YAML against real footage (the `gow.yaml` comments are an excellent record of how expensive that is). Build `clipper bootstrap <video>`: sample frames, have the LLM identify HUD regions (killfeed, banner, subtitle band, timer) and propose a draft profile with regions and signal kinds, then immediately score it against a small labelled set from 7.1. Turns days of tuning into an hour of correction, and is what lets you scale past three games.

### 7.4 Cross-session dedupe

Perceptual hash a few frames per rendered clip and flag near-duplicates. Two recordings of the same boss attempt currently produce two clips with no relation to each other, and reposting near-identical content is a real distribution penalty.

---

## 8. Recommended order

| # | Work | Why here | Rough size |
|---|---|---|---|
| **0** | Link Facebook Page, Meta app + tokens | Unlocks disk upload, competitor data, audio. Blocks everything. | ½ day |
| **1** | `pkg/publish` + `publication`/`metric_snapshot` + insights poller + `clipper serve` library UI | The join key and the toil removal. Prerequisite for 3 and 4. | 1–2 weeks |
| **2** | Retention render pass: use `crop_focus`, burn ASR captions, hook overlay, loudnorm, fix double-encode + drawtext escaping + cover shape | Biggest per-clip quality jump; inputs already exist | 1 week |
| **3** | Variant renderer + trial-reel A/B harness | Turns guesses into measured decisions | 1 week |
| **4** | YouTube-corpus + `business_discovery` intel → reviewed signal brief | Generates hypotheses that 3 can now test | 1–2 weeks |
| **5** | Eval harness, rejection mining, profile bootstrap, dedupe | Makes scoring changes safe and new games cheap | ongoing |

Do **2 before 4**. A text hook, burned captions, a tight crop and normalised audio are known-good improvements that need no research at all; competitor analysis is how you find the *non*-obvious 20% and it's much more work. Doing 4 first means spending two weeks studying creators to discover you should add overlay text.

## 9. Things I'd explicitly not build

- **Chrome-driven Instagram uploads as the primary path.** Ban risk to the account you're growing, for something the official API does. Keep the browser only for the human-in-loop trending-audio case.
- **Scraping Instagram.** Terms violation, unstable, and `business_discovery` + YouTube covers the legitimate need.
- **An ML model for "will this clip pop"** before you have a few hundred paired trial results. Rank ordinally, tune profile weights, wait.
- **Multi-game breadth** before Phase 2 is done. Three profiles is enough to prove the architecture; more games multiply the tuning cost you haven't automated yet.
- **A web frontend build step for the library UI.** The plain-HTML-plus-vanilla-JS decision in `pkg/review` is a good one. Don't regress it.

---

## Sources

- [Instagram Content Publishing — Meta for Developers](https://developers.facebook.com/docs/instagram-platform/content-publishing/) — REELS containers, resumable upload, trial reels, 100 posts/24h, container status codes
- [Business Discovery API — Meta for Developers](https://developers.facebook.com/docs/instagram-api/business-discovery) — available competitor fields
- [IG Media reference — Meta for Developers](https://developers.facebook.com/docs/instagram-platform/reference/instagram-media/)
- [Instagram Reels API developer guide 2026 — Phyllo](https://www.getphyllo.com/post/a-complete-guide-to-the-instagram-reels-api) — 90s API cap, insights metrics
- [Instagram Reels API publishing guide — Postproxy](https://postproxy.dev/blog/instagram-reels-api-publishing-guide/) — API music library limitation, 300 MB via API
- [Instagram Music API for Reels audio IDs — bundle.social](https://bundle.social/instagram-music-api) — audio search / `audio_id`, Facebook Login requirement
- [Instagram algorithm signals 2026 — ValueYourNetwork](https://www.valueyournetwork.com/en/instagram-algorithm-signals/) — watch time, shares/reach over likes
- [YouTube Data API quota limits 2026 — Phyllo](https://www.getphyllo.com/post/youtube-api-limits-how-to-calculate-api-usage-cost-and-fix-exceeded-api-quota) and [SocialCrawl](https://www.socialcrawl.dev/blog/youtube-data-api-2026) — 10,000 units/day, search vs read costs
- [Is Instagram automation safe in 2026 — CreatorFlow](https://creatorflow.so/blog/is-instagram-automation-safe-2026/) and [Mixpost](https://mixpost.app/blog/automate-instagram-posts-safely) — official API vs browser automation risk
