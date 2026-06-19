// harb keyboard nav — minimal, no deps. Loaded from base.html.
//
// Global (every authenticated page):
//   ?      → toggle keyboard-help overlay
//   Esc    → close overlay
//   u      → up the hierarchy (entry view → parent feed; any other
//            authenticated page → home /ui/)
//   ← / u  → up the hierarchy (also: feed view → home)
//   N / n  → toggle "show unread only" filter (home + per-feed views)
//
// On any list view — home feeds (/ui/) and entry lists
// (/ui/feed, /ui/all, /ui/starred):
//   j / ↓  → focus next row
//   k / ↑  → focus previous row
//   Enter  → open the focused row's primary link
//   gg     → first row, G → last row
//
// Home master-detail (wide screens ≥ 64em only):
//   j / k  → move feed selection; the selected feed's entries are
//            previewed in the right-hand #feed-pane via htmx (?panel=1)
//   r      → mark ALL entries of the selected feed read (in place)
//   → / Enter → drill in to /ui/feed?id=… (full entries + article view)
//   ← / u  → back out to the feeds list (home)
//   On narrow screens the home view stays a plain feeds list and these
//   keys are inert (Enter follows the feed link as a full-page nav).
//
// Auto-select-first (wide screens ≥ 64em only):
//   When a split view loads (or is restored from bfcache) with at
//   least one visible row and nothing already selected, the first
//   visible row is auto-selected so the right pane is never empty:
//     - home (/ui/)       → first feed row selected, its entries
//                           previewed into #feed-pane
//     - entry list views  → first entry row selected, its article
//                           previewed into #detail-pane
//   This is a fallback only: an existing restored selection (via the
//   focusedHash logic) is preferred and never overridden. It respects
//   the unread-only / tag filters ("first visible row" = the first row
//   per the rows() helper) and is inert on narrow screens. Previewing
//   the first article follows the normal ~0.7 s dwell auto-mark-read
//   rule (it is a genuinely-shown entry); merely loading a list never
//   marks anything else read.
//
// Entry-list additions:
//   r      → toggle row's read state
//   s      → toggle row's star
//   R      → mark all read (if a "mark all read" button is present)
//
// On the entry view (/ui/entry):
//   r      → mark read/unread
//   s      → star/unstar
//   o / →  → open source (article) in new window
//   u      → back to parent feed
//
// Behaviour:
//   - The entry view auto-marks the entry as read after ~0.7 s of
//     dwell — both on the standalone entry page and on entries loaded
//     into the split-panel detail pane via htmx. In split mode the
//     matching list row is patched in place too (no scroll loss).
//     Navigating away or toggling read/star manually earlier cancels.
//   - When a page is restored from the browser's back/forward cache
//     (e.g. you hit Back from an entry view), it is force-reloaded
//     so read/star toggles you made are reflected without F5.
//
// Touch gestures (touch devices, mirrors the keyboard nav). This
// REVISES the v0.7.7 mapping: the two directions are flipped and the
// left-swipe action is extended to feed rows and the article view.
//
//   swipe RIGHT (finger left→right, ">")  → up the hierarchy (same as
//       `u` / ←: standalone entry → parent feed; any other non-home
//       page → universal up). Page-level; never fires from form fields
//       or the help overlay. (This was swipe-LEFT in v0.7.7.)
//
//   swipe LEFT  (finger right→left, "<")  → context action, scoped to
//       whichever element the gesture STARTED on:
//         · entry row (ul.entries li): short swipe toggles READ,
//           long swipe toggles STAR (clicks the row's .readbtn /
//           .starbtn so htmx + OOB/focus patching keep working).
//         · article view (.entry-full — standalone page OR inside
//           #detail-pane): short → toggle read, long → toggle star
//           (clicks .actions .readbtn / .starbtn). A swipe that starts
//           on a link/button inside the article is left alone, and a
//           swipe that starts inside a horizontally-scrollable element
//           (wide code block / table) scrolls that element instead.
//         · feed row (ul.feeds li): short swipe "enters" / drills into
//           the feed (clicks the feed link, same as Enter / tapping it).
//
//   Live preview: while swiping LEFT the target element translates a
//   little under the finger and reveals WHICH action will fire if you
//   release now — a READ hint (●) past the short threshold switching to
//   a STAR hint (★) past the long threshold for rows/articles, or an
//   "open" hint (→) for feed rows. Releasing before the short threshold
//   (or letting it become a vertical scroll) fires nothing and snaps
//   back with no tint.
//
//   The axis is locked to horizontal only once |dx| dominates |dy| and
//   passes a small slop, so vertical scrolling and taps are unaffected;
//   preventDefault is called ONLY after that horizontal commit (which is
//   why touchmove is registered with { passive:false }).
(function () {
  "use strict";

  // ---- helpers -----------------------------------------------------
  const $  = (sel, root) => (root || document).querySelector(sel);
  const $$ = (sel, root) => Array.from((root || document).querySelectorAll(sel));

  // BASE is the relative prefix that, resolved against the current
  // page, points at the /ui/ root. The server emits it via
  // <html data-ui-base="..."> (see internal/ui/templates/base.html).
  // We never hard-code an absolute /ui path — under a path-prefix
  // deployment (e.g. Tailscale Funnel --set-path=/rss) the absolute
  // UI root is /rss/ui/, and an absolute /ui/... reference 404s.
  const BASE = document.documentElement.dataset.uiBase || "./";
  // Absolute pathname of the /ui/ root for this deployment, used to
  // tell whether a same-origin referrer is a list page.
  const UI_ROOT = new URL(BASE, window.location.href).pathname;
  // Build URLs relative to the /ui/ root. `seg` must be relative
  // (no leading slash).
  const uiURL = (seg) => BASE + seg;

  const inEditable = (e) => {
    const t = e.target;
    if (!t) return false;
    if (t.matches && t.matches("input, textarea, select")) return true;
    if (t.isContentEditable) return true;
    return false;
  };

  // ---- capture "return feed" on the feed entries page --------------
  // On a per-feed entries page (Scope=="feed") the manage panel carries
  // input#feed-url whose value IS the feed URL (== ScopeID == the home
  // row's ?id). Remember it (with the feed title from the h1's text and
  // a timestamp) so that when the user backs out to the home master-
  // detail we can restore selection to THIS feed instead of auto-
  // selecting the first one. Updated on every feed visit (desirable).
  // input#feed-url is only emitted for feed scope, so /ui/all and
  // /ui/starred never set it. Storage is wrapped (private mode throws).
  (function captureReturnFeed() {
    const inp = $("#feed-url");
    if (!inp || !inp.value) return;
    const id = inp.value;
    let title = "";
    const h1 = $("h1");
    if (h1) {
      h1.childNodes.forEach((n) => { if (n.nodeType === 3) title += n.textContent; });
      title = title.trim();
    }
    try {
      sessionStorage.setItem(
        "harb.returnFeed",
        JSON.stringify({ id: id, title: title, ts: Date.now() })
      );
    } catch (_) { /* private mode — best effort */ }
  })();

  // ---- global: ? help overlay --------------------------------------
  const help = $("#kbd-help");
  const backdrop = $("#kbd-backdrop");
  const helpOpen = () => help && !help.hasAttribute("hidden");
  const toggleHelp = (show) => {
    if (!help) return;
    const open = show === undefined ? !helpOpen() : show;
    if (open) {
      help.removeAttribute("hidden");
      backdrop && backdrop.removeAttribute("hidden");
    } else {
      help.setAttribute("hidden", "");
      backdrop && backdrop.setAttribute("hidden", "");
    }
  };
  if (backdrop) backdrop.addEventListener("click", () => toggleHelp(false));

  // ---- theme toggle (auto → dark → light → auto) -------------------
  // Persisted client-side in localStorage so it survives reloads
  // without needing a server roundtrip. The inline <head> bootstrap
  // applies the stored value before paint to avoid theme flash.
  const themeBtn = $("#theme-toggle");
  if (themeBtn) {
    const cycle = ["auto", "dark", "light"];
    const glyph = { auto: "◐", dark: "●", light: "○", sepia: "◑" };
    const label = {
      auto:  "theme: auto (follows system) — click for dark",
      dark:  "theme: dark — click for light",
      light: "theme: light — click for auto",
      sepia: "theme: sepia — click for auto",
    };
    const current = () => document.documentElement.getAttribute("data-theme") || "auto";
    const paint = () => {
      const t = current();
      themeBtn.textContent = glyph[t] || "◐";
      themeBtn.title = label[t] || ("theme: " + t);
    };
    paint();
    themeBtn.addEventListener("click", function () {
      const t = current();
      const i = cycle.indexOf(t);
      const next = cycle[(i + 1) % cycle.length] || "auto";
      document.documentElement.setAttribute("data-theme", next);
      try { localStorage.setItem("harb.theme", next); } catch (e) { /* ignore */ }
      paint();
    });
  }

  // ---- refresh on back/forward bfcache restore ---------------------
  // Without this, hitting "back" from an entry view shows the previous
  // list snapshot — read/star toggles done on the entry don't appear
  // until you F5. Reload when the page is restored from bfcache.
  window.addEventListener("pageshow", function (e) {
    if (e.persisted) window.location.reload();
  });

  // ---- universal "u" — up the hierarchy ----------------------------
  // Entry view has its own `u` handler (back to parent feed) below.
  // On any other authenticated page that isn't already the home feed
  // list, `u` goes to /ui/. When we arrived from /ui/ (with whatever
  // filter the user had), prefer history.back() so the pill state and
  // scroll position are preserved.
  const sameOriginRef = function () {
    const r = document.referrer;
    if (!r) return null;
    try {
      const u = new URL(r);
      if (u.origin !== window.location.origin) return null;
      return u;
    } catch (_) { return null; }
  };
  // isListPath: true when a same-origin referrer pathname is one of the
  // list pages under the /ui/ root (home, feed, all, starred).
  const isListPath = function (p) {
    if (p === UI_ROOT || p + "/" === UI_ROOT) return true;
    if (!p.startsWith(UI_ROOT)) return false;
    const tail = p.slice(UI_ROOT.length);
    return tail === "feed" || tail === "all" || tail === "starred";
  };
  // universalUp: walk up the hierarchy from any page that ISN'T the
  // standalone entry view. In split-panel mode the entry detail lives
  // inside #detail-pane; that doesn't count as "on the entry view" — we
  // still walk up to /ui/. Returns true when it navigated (so a keyboard
  // caller knows to preventDefault).
  const universalUp = function () {
    const article = $(".entry-full");
    if (article && !article.closest("#detail-pane")) return false;
    const path = window.location.pathname;
    if (path === UI_ROOT || path + "/" === UI_ROOT) return false;  // already at top
    const ref = sameOriginRef();
    if (ref && (ref.pathname === UI_ROOT || ref.pathname + "/" === UI_ROOT)) {
      window.history.back();
    } else {
      window.location.href = BASE;
    }
    return true;
  };
  // entryUp: standalone entry view → back to the parent feed. Prefer
  // history.back() when we came from a same-origin list page (restores
  // pill/scroll state); else follow the canonical parent-feed link in
  // the meta line.
  const entryUp = function () {
    const ref = sameOriginRef();
    if (ref && isListPath(ref.pathname)) {
      window.history.back();
    } else {
      const a = $(".meta a[href*='feed?id=']");
      if (a) window.location.href = a.href;
    }
  };
  // goUp: the single page-level "up the hierarchy" action shared by the
  // `u` / ← keys AND the swipe-left touch gesture. On the standalone
  // entry view it goes to the parent feed (entryUp); everywhere else it
  // runs the universal behaviour (universalUp). Returns true if it acted.
  const goUp = function () {
    const article = $(".entry-full");
    if (article && !article.closest("#detail-pane")) { entryUp(); return true; }
    return universalUp();
  };
  document.addEventListener("keydown", function (e) {
    if (inEditable(e) || helpOpen()) return;
    if (e.key !== "u" && e.key !== "ArrowLeft") return;
    if (universalUp()) e.preventDefault();
  });

  // ---- N — toggle "show unread only" filter ------------------------
  // Wherever the page renders an `a.filter` pill (home and per-feed
  // entry list), pressing N navigates to the URL the pill points to.
  // No-op on pages without the pill (entry view, /ui/all, /ui/starred).
  document.addEventListener("keydown", function (e) {
    if (inEditable(e) || helpOpen()) return;
    if (e.key !== "N" && e.key !== "n") return;
    const pill = $("a.filter");
    if (!pill) return;
    window.location.href = pill.href;
    e.preventDefault();
  });

  document.addEventListener("keydown", function (e) {
    if (inEditable(e)) return;
    if (e.key === "?") { toggleHelp(); e.preventDefault(); return; }
    if (e.key === "Escape" && helpOpen()) { toggleHelp(false); e.preventDefault(); }
  }, true); // capture so help-toggle wins over page handlers

  // ---- list-view nav (works on home feeds list AND entry lists) ----
  //
  // We pick whichever list is visible: ul.entries (entry rows) takes
  // precedence; ul.feeds (home page) is the fallback. A "row" is just
  // an <li> with a primary <a> we can navigate to on Enter.
  const entryList = $("ul.entries");
  const feedLists = $$("ul.feeds");
  const list = entryList || (feedLists.length ? feedLists[0] : null);
  const isEntryList = !!entryList;

  if (list) {
    const rows = () => {
      if (isEntryList) {
        return $$("li", list).filter((li) => !li.classList.contains("empty"));
      }
      // Home page: collect rows across every visible ul.feeds (one per
      // tag group). Skip rows inside a hidden (collapsed) ul.
      const out = [];
      feedLists.forEach((ul) => {
        if (ul.hasAttribute("hidden")) return;
        $$("li", ul).forEach((li) => {
          if (!li.classList.contains("empty")) out.push(li);
        });
      });
      return out;
    };
    let idx = -1;
    // Track the focused row by its hash (the part of id="entry-<hash>"
    // after the prefix) in addition to its index. Index alone is
    // fragile across htmx swaps: outerHTML replacement of a list row
    // wipes the .kb-focus class, and if anything ever reorders rows
    // the index becomes wrong. Hash survives both.
    let focusedHash = "";
    const rowHash = (li) => {
      const id = li && li.id;
      return id && id.indexOf("entry-") === 0 ? id.slice("entry-".length) : "";
    };
    // Debounced "auto-preview the focused row into #detail-pane" so
    // holding j/k doesn't fire one htmx request per repeat. Only active
    // on entry-list pages (not the home feed-list) and only when the
    // viewport is wide enough that the split-panel is visible.
    const wideScreen = () => window.matchMedia("(min-width: 64em)").matches;
    // Open an entry row's title link. On wide screens we swap the entry
    // detail into the split-panel via htmx.ajax; on narrow screens we
    // follow the native href (full-page nav). This decision MUST live in
    // JS, not in an hx-trigger media-query filter on the anchor: htmx
    // calls preventDefault() on <a href> clicks *before* it evaluates
    // the trigger filter, so a filtered-out click on mobile cancelled
    // the native navigation AND fired no request — the tap was dead and
    // there was no way to open an article on mobile at all.
    const openEntry = (a) => {
      if (!a) return;
      const href = a.getAttribute("href");
      if (href && wideScreen() && window.htmx) {
        const sep = href.indexOf("?") >= 0 ? "&" : "?";
        try { htmx.ajax("GET", href + sep + "panel=1", "#detail-pane"); return; }
        catch (_) { /* fall through to full-page nav */ }
      }
      window.location.href = a.href;
    };
    let previewTimer = null;
    const schedulePreview = () => {
      if (!wideScreen()) return;
      // On entry-list pages we preview the focused entry into the
      // article pane (#detail-pane); on the home feed-list we preview
      // the focused feed's entry list into the master-detail pane
      // (#feed-pane). Both are gated on the target pane existing in the
      // DOM — it is display:none below 64em, so this is a no-op on
      // narrow screens and on any page without the pane.
      const paneSel = isEntryList ? "#detail-pane" : "#feed-pane";
      if (!document.querySelector(paneSel)) return;
      if (previewTimer !== null) clearTimeout(previewTimer);
      previewTimer = setTimeout(function () {
        previewTimer = null;
        if (idx < 0) return;
        const cur = rows()[idx];
        if (!cur) return;
        // Entry rows expose a.entry-link; home feed rows expose the
        // feed link as their first/primary anchor.
        const a = isEntryList ? cur.querySelector("a.entry-link") : cur.querySelector("a");
        if (!a || !window.htmx) return;
        const href = a.getAttribute("href");
        // htmx.ajax resolves the URL relative to the document, which
        // matches what a native click on the anchor would do. The
        // promise rejection on aborted requests is harmless here.
        const sep = href.indexOf("?") >= 0 ? "&" : "?";
        try { htmx.ajax("GET", href + sep + "panel=1", paneSel); } catch (_) { /* */ }
      }, 140);
    };
    // Mark every entry of the keyboard-selected feed read (home master-
    // detail only). We POST the existing feed-scope mark-all-read
    // endpoint directly — not via the .markall click interceptor — then
    // zero the row's unread count and refresh the preview pane in place,
    // so focus and scroll are preserved. Returns true when it acted.
    // Subtract `n` from the bare-integer .count badge in `el` (tag group
    // header / sidebar / feed row), clamped at zero.
    const decCount = (el, n) => {
      if (!el) return;
      const v = Math.max(0, (parseInt(el.textContent, 10) || 0) - n);
      el.textContent = String(v);
    };
    // After a feed row is marked read in place, draw down the aggregate
    // unread counters that included it: its tag group header, the
    // matching sidebar tag entry, the always-present sidebar "all"
    // entry, and the "(N unread)" page total. Keeps the home page
    // consistent without a reload.
    const decAncestorCounts = (row, n) => {
      const section = row.closest(".feed-group");
      const tag = section ? (section.getAttribute("data-tag") || "") : "";
      if (section) decCount(section.querySelector(".feed-group-head .count"), n);
      document.querySelectorAll(".taglist li").forEach((li) => {
        const a = li.querySelector("a");
        if (!a) return;
        const href = a.getAttribute("href") || "";
        const qi = href.indexOf("?tag=");
        const liTag = qi >= 0 ? decodeURIComponent(href.slice(qi + 5)) : "";
        // liTag === "" is the "all" entry (href "./") — always affected.
        if (liTag === "" || liTag === tag) decCount(li.querySelector(".count"), n);
      });
      const tot = document.querySelector("h1 small");
      if (tot) {
        const cur = (tot.textContent.match(/\d+/) || ["0"])[0];
        const v = Math.max(0, (parseInt(cur, 10) || 0) - n);
        tot.textContent = "(" + v + " unread)";
      }
    };
    const markSelectedFeedRead = () => {
      if (isEntryList || !wideScreen() || idx < 0) return false;
      const row = rows()[idx];
      if (!row) return false;
      const a = row.querySelector("a");
      if (!a || !window.htmx) return false;
      let id;
      try { id = new URL(a.href).searchParams.get("id"); } catch (_) { return false; }
      if (!id) return false;
      fetch(uiURL("mark-all-read?scope=feed&id=" + encodeURIComponent(id)), {
        method: "POST", credentials: "same-origin",
      }).then(function (resp) {
        if (!resp.ok) return;
        const c = row.querySelector(".count");
        const prev = c ? (parseInt(c.textContent, 10) || 0) : 0;
        if (c) c.textContent = "0";
        // Marking the feed read in place must also draw down every
        // aggregate counter that included it — the tag group header, the
        // matching sidebar tag entry, the sidebar "all" entry, and the
        // page total — otherwise those stay stale until a full reload.
        if (prev > 0) decAncestorCounts(row, prev);
        const href = a.getAttribute("href");
        const sep = href.indexOf("?") >= 0 ? "&" : "?";
        try { htmx.ajax("GET", href + sep + "panel=1", "#feed-pane"); } catch (_) { /* */ }
      }).catch(function () { /* network hiccup — user can retry */ });
      return true;
    };
    const focusRow = (i) => {
      const all = rows();
      if (all.length === 0) return;
      if (i < 0) i = 0;
      if (i >= all.length) i = all.length - 1;
      all.forEach((r, j) => r.classList.toggle("kb-focus", j === i));
      idx = i;
      focusedHash = rowHash(all[i]);
      all[i].scrollIntoView({ block: "nearest" });
      schedulePreview();
    };
    // Re-apply kb-focus by hash whenever the list mutates. Handles
    // three cases that all wipe the class on the focused row:
    //   1. row read/star toggle returns a fresh <li> via outerHTML
    //   2. detail-pane read/star toggle issues an OOB row patch
    //   3. any future list-mutating swap
    // We also re-sync idx so j/k continue from the new position.
    const reapplyFocus = function () {
      if (!focusedHash) return;
      const all = rows();
      for (let i = 0; i < all.length; i++) {
        if (rowHash(all[i]) === focusedHash) {
          all[i].classList.add("kb-focus");
          idx = i;
          return;
        }
      }
    };
    // Auto-select the first visible row as a fallback when the split
    // view loads (or is restored from bfcache, which force-reloads) and
    // nothing is selected yet — so the right pane never lands empty.
    // Wide-screen only and gated on the target pane existing. We defer
    // to any selection already restored by reapplyFocus (idx >= 0): the
    // focusedHash restoration wins; this only fires when idx < 0. The
    // first "visible" row is rows()[0] — rows() already drops empty and
    // hidden (collapsed-group / filtered-out) rows, so unread-only and
    // tag filters are honoured. focusRow(0) sets focus AND schedules the
    // preview (entry article into #detail-pane, feed entries into
    // #feed-pane). Empty lists leave the pane's placeholder untouched.
    const autoSelectFirst = function () {
      if (!wideScreen() || idx >= 0) return;
      const paneSel = isEntryList ? "#detail-pane" : "#feed-pane";
      if (!document.querySelector(paneSel)) return;
      if (rows().length === 0) return;
      focusRow(0);
    };
    // Restore selection to the feed we just backed out of (home master-
    // detail only). Preferred over autoSelectFirst when sessionStorage
    // carries a fresh "harb.returnFeed" (set on the feed page). Returns
    // true when it took selection, so the caller skips autoSelectFirst.
    //
    //   row present  → focusRow on it (kb-focus + entries into #feed-pane).
    //   row absent + fresh (≤5 min) → the feed is now empty (0 unread
    //     under the unread-only filter) so the server omitted its row.
    //     Inject a synthetic, dimmed li.returning carrying the feed link
    //     + a 0 count and select THAT: focusRow previews the feed's
    //     (all-caught-up) entries into #feed-pane, and Right/Enter on it
    //     re-enters the feed. rows() includes it (non-.empty li).
    //   row absent + stale → fall through (the feed may be gone).
    //
    // We keep returnFeed after a synthetic restore so repeated home
    // visits keep the selection (cleaner than a one-shot vanish); the
    // 5-minute recency window still guards against phantom rows for
    // feeds deleted long ago.
    const RETURN_FRESH_MS = 5 * 60 * 1000;
    const rowFeedId = function (li) {
      const a = li.querySelector("a");
      if (!a) return null;
      try { return new URL(a.href, window.location.href).searchParams.get("id"); }
      catch (_) { return null; }
    };
    const restoreReturnFeed = function () {
      if (isEntryList || !wideScreen() || idx >= 0) return false;
      if (!document.querySelector("#feed-pane")) return false;
      let rf;
      try {
        const raw = sessionStorage.getItem("harb.returnFeed");
        if (!raw) return false;
        rf = JSON.parse(raw);
      } catch (_) { return false; }
      if (!rf || !rf.id) return false;
      const all = rows();
      for (let i = 0; i < all.length; i++) {
        if (rowFeedId(all[i]) === rf.id) { focusRow(i); return true; }
      }
      // Not in the list — only synthesise a row for a recent visit.
      if (!rf.ts || (Date.now() - rf.ts) > RETURN_FRESH_MS) return false;
      if (!feedLists.length) return false;
      const ul = feedLists[0];
      const li = document.createElement("li");
      li.className = "returning";
      const a = document.createElement("a");
      a.setAttribute("href", "feed?id=" + encodeURIComponent(rf.id));
      a.textContent = rf.title || rf.id;
      li.appendChild(a);
      li.appendChild(document.createTextNode(" "));
      const span = document.createElement("span");
      span.className = "count";
      span.textContent = "0";
      li.appendChild(span);
      ul.appendChild(li);
      const after = rows();
      const j = after.indexOf(li);
      if (j < 0) { li.remove(); return false; }
      focusRow(j);
      return true;
    };
    // Mouse/tap click on a row → treat it as the new keyboard focus, so
    // subsequent j/k navigation continues from the clicked row. Event
    // delegation on the list lets the row's icon-buttons keep their own
    // click handlers; we track which row was hit and, for the title
    // link, drive the open ourselves (panel swap vs full-nav) — see
    // openEntry for why this can't be an hx-trigger media filter.
    list.addEventListener("click", function (e) {
      const li = e.target.closest("li");
      if (!li || li.classList.contains("empty") || !list.contains(li)) return;
      const all = rows();
      const i = all.indexOf(li);
      if (i < 0) return;
      all.forEach((r, j) => r.classList.toggle("kb-focus", j === i));
      idx = i;
      focusedHash = rowHash(li);
      const link = e.target.closest("a.entry-link");
      if (link && list.contains(link)) {
        // Honour modifier-clicks (cmd/ctrl/shift, middle-click) so
        // "open in new tab/window" keeps working on desktop.
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
        e.preventDefault();
        openEntry(link);
      }
    });
    const clickInRow = (sel) => {
      if (idx < 0) return;
      const btn = rows()[idx].querySelector(sel);
      if (btn) btn.click();
    };
    // Open (in a new tab) the source link of the article currently shown
    // in the wide-screen detail pane (entry-list master-detail view).
    // The standalone `.entry-full` keydown handler below only wires up
    // when an article exists at page load; on the split view the article
    // is htmx-swapped into #detail-pane later, so its `o`/→ shortcut
    // never bound. Handle it here instead. Returns true when it acted.
    const openDetailSource = () => {
      const pane = document.querySelector("#detail-pane");
      if (!pane) return false;
      const src = pane.querySelector(".entry-full a.source-link");
      if (!src) return false;
      const href = src.getAttribute("href");
      if (!href) return false;
      window.open(href, "_blank", "noopener");
      return true;
    };
    let lastG = 0;
    // After any htmx swap on this page (row toggle outerHTML, OOB
    // patches from detail-pane toggles, etc.) re-apply kb-focus to
    // the row whose hash matches the tracked focusedHash. Listen on
    // both afterSwap (synchronous, immediate) and afterSettle (covers
    // any settling-phase reflow) so the marker survives reliably.
    document.addEventListener("htmx:afterSwap", reapplyFocus);
    document.addEventListener("htmx:afterSettle", reapplyFocus);
    document.addEventListener("keydown", function (e) {
      if (inEditable(e) || helpOpen()) return;
      switch (e.key) {
        case "j": case "ArrowDown": focusRow(idx + 1); e.preventDefault(); break;
        case "k": case "ArrowUp":   focusRow(idx - 1); e.preventDefault(); break;
        case "ArrowRight":
          // Home master-detail: "drill in" to the selected feed's full
          // entries+article view. Equivalent to Enter on a feed row.
          if (!isEntryList && idx >= 0) {
            const a = rows()[idx].querySelector("a");
            if (a) { a.click(); e.preventDefault(); }
          } else if (isEntryList && openDetailSource()) {
            // Entry-list split view: open the detail pane article's
            // source — mirrors `o` / → on the standalone entry page.
            e.preventDefault();
          }
          break;
        case "o":
          // Entry-list split view: open the detail pane article's source
          // in a new tab. (Standalone entry page handles `o` separately.)
          if (isEntryList && openDetailSource()) e.preventDefault();
          break;
        case "Enter":
          if (idx >= 0) {
            const row = rows()[idx];
            const link = row.querySelector("a.entry-link");
            if (link) {
              // Entry rows: openEntry picks panel-swap (wide) vs
              // full-page nav (narrow) itself.
              openEntry(link); e.preventDefault();
            } else {
              // Home feed-list rows: just follow the feed link.
              const a = row.querySelector("a");
              if (a) { a.click(); e.preventDefault(); }
            }
          }
          break;
        case "g":
          if (Date.now() - lastG < 500) { focusRow(0); lastG = 0; }
          else { lastG = Date.now(); }
          break;
        case "G": focusRow(rows().length - 1); break;
        case "r":
          // Entry list: toggle the focused row's read state. Home
          // master-detail: mark the whole selected feed read.
          if (isEntryList) clickInRow(".readbtn");
          else if (markSelectedFeedRead()) e.preventDefault();
          break;
        case "s": if (isEntryList) clickInRow(".starbtn"); break;
        case "R":
          if (isEntryList) {
            // Click the button (not form.submit()) so our click
            // interceptor below fires and history.back()s us to the
            // origin page, preserving its filter/tag state.
            const b = $(".markall");
            if (b) b.click();
          }
          break;
      }
    });

    // Initial fallback selection — runs once after wiring, after any
    // synchronous reapplyFocus from load-time swaps. The idx<0 guard
    // inside means a restored selection is preferred. On the home
    // master-detail we first try to restore the feed we just backed out
    // of (restoreReturnFeed); only if that doesn't take do we auto-
    // select the first feed.
    if (!restoreReturnFeed()) autoSelectFirst();
  }

  // ---- entry view --------------------------------------------------
  //
  // patchReadBtn updates a read-toggle button in place: the glyph
  // (●/○), the tooltip + aria-label, and the hx-post URL's state=
  // parameter so the next click toggles the correct direction. htmx
  // re-reads hx-post lazily on each event, so mutating the attribute
  // is enough.
  const patchReadBtn = function (btn, isRead) {
    if (!btn) return;
    btn.textContent = isRead ? "○" : "●";
    btn.title = isRead ? "mark unread" : "mark read";
    btn.setAttribute("aria-label", isRead ? "mark unread" : "mark read");
    const hxp = btn.getAttribute("hx-post");
    if (hxp) {
      btn.setAttribute("hx-post", hxp.replace(/state=\d/, "state=" + (isRead ? "0" : "1")));
    }
  };
  //
  // wireArticle attaches the dwell-based auto-mark-read timer and a
  // cancel hook to whatever .entry-full element is passed in. It is
  // called once at document load for the standalone /ui/entry page,
  // and again on every htmx:afterSwap into #detail-pane so entries
  // loaded into the split-panel pane get the same behaviour.
  let activeTimer = null;
  const wireArticle = function (article) {
    if (!article) return;
    if (activeTimer !== null) { clearTimeout(activeTimer); activeTimer = null; }
    if (article.classList.contains("read")) return;
    const m = article.id.match(/^entry-detail-(.+)$/);
    if (!m) return;
    const hash = m[1];
    const dwellMs = 700;
    activeTimer = setTimeout(function () {
      activeTimer = null;
      // Resolve via uiURL so this fetch works correctly under any
      // external URL prefix (e.g. /rss-test/ui/ on a tailscale
      // funnel). An absolute /ui/entry/read would silently 404 on a
      // prefixed deployment.
      fetch(uiURL("entry/read?id=" + encodeURIComponent(hash) + "&state=1"), {
        method: "POST",
        credentials: "same-origin",
      }).then(function (r) {
        if (!r.ok) return;
        // Patch the pane's article in place — re-fetching via htmx
        // would re-swap the entire pane and reset scroll position
        // (jarring at 0.7 s dwell), so we DOM-patch directly.
        article.classList.add("read");
        patchReadBtn(article.querySelector(".actions .readbtn"), true);
        // Also patch the matching list row so the left pane reflects
        // the new read state without a server round trip.
        const row = document.getElementById("entry-" + hash);
        if (row) {
          row.classList.add("read");
          patchReadBtn(row.querySelector(".readbtn"), true);
        }
      }).catch(function () { /* network hiccup — user can still click */ });
    }, dwellMs);
    const cancel = function () {
      if (activeTimer !== null) { clearTimeout(activeTimer); activeTimer = null; }
    };
    window.addEventListener("pagehide", cancel, { once: true });
    const actions = article.querySelector(".actions");
    if (actions) actions.addEventListener("click", cancel, true);
  };

  const article = $(".entry-full");
  if (article) {
    wireArticle(article);

    document.addEventListener("keydown", function (e) {
      if (inEditable(e) || helpOpen()) return;
      switch (e.key) {
        case "r": {
          const b = $(".actions button:nth-of-type(1)");
          if (b) b.click();
          break;
        }
        case "s": {
          const b = $(".actions button:nth-of-type(2)");
          if (b) b.click();
          break;
        }
        case "u": {
          // Shared with the swipe-left gesture — see entryUp above.
          entryUp();
          break;
        }
        case "o":
        case "ArrowRight": {
          // Open the article's original source in a new window/tab.
          // No-op when the entry has no source link.
          const src = article.querySelector("a.source-link");
          if (src) {
            const href = src.getAttribute("href");
            if (href) window.open(href, "_blank", "noopener");
          }
          break;
        }
      }
    });
  }

  // ---- touch swipe gestures ----------------------------------------
  //
  // Touch-only gestures that mirror the keyboard nav, for narrow /
  // touch devices. Implemented with touchstart/move/end (clean, no
  // Pointer-Event polyfilling) and dependency-free.
  //
  //   SWIPE RIGHT (finger left→right) → up the hierarchy. Page-level:
  //       runs the SAME goUp() the `u` / ← keys use (standalone entry
  //       view → parent feed; any other non-home page → universal up).
  //   SWIPE LEFT (finger right→left) → context action scoped to the
  //       element the gesture started on:
  //         entry row  : short → toggle READ (.readbtn),
  //                      long  → toggle STAR (.starbtn)
  //         article    : short → toggle READ (.actions .readbtn),
  //                      long  → toggle STAR (.actions .starbtn)
  //         feed row   : short → enter / drill into the feed (its link)
  //       Using the elements' existing buttons/links keeps htmx + the
  //       OOB/focus patching working unchanged. The element translates a
  //       little during the drag, previews the pending action, and snaps
  //       back on release.
  //
  // Axis lock: we only commit to a horizontal swipe once |dx| clearly
  // dominates |dy| and passes a small slop — until then the browser is
  // left to scroll vertically. preventDefault is called ONLY after that
  // commit, which is why touchmove is registered with { passive:false }.
  // Taps never lock the axis, so taps/button-clicks/link-opens are
  // unaffected. The gesture never starts from a form field or while the
  // help overlay is open. A left-swipe that starts inside a
  // horizontally-scrollable element (wide code block / table in an
  // article) is left to scroll that element natively.
  (function () {
    const SLOP = 10;        // px of travel before we decide the axis
    const SHORT = 40;       // px left-swipe to toggle read / enter feed
    const LONG = 120;       // px left-swipe to toggle star instead
    const CAP = 150;        // max px the element visually translates
    const BACK = 60;        // px right-swipe to trigger up-the-hierarchy

    let sx = 0, sy = 0;
    let tracking = false;
    let axis = null;        // null | 'h' | 'v'
    let actionEl = null;    // element captured at touchstart (left-swipe)
    let kind = null;        // null | 'entry' | 'feed' | 'article'
    let letScroll = false;  // touch started in a horizontal scroller

    // Walk ancestors from `node` up to (but not including) `stop`,
    // looking for an element that can scroll horizontally and currently
    // has room to. Used so a left-swipe that begins inside a wide code
    // block / table scrolls that element instead of firing an action.
    const inHScroll = function (node, stop) {
      let el = node;
      while (el && el !== stop && el.nodeType === 1) {
        const ox = window.getComputedStyle(el).overflowX;
        if ((ox === "auto" || ox === "scroll") && el.scrollWidth > el.clientWidth + 1) return true;
        el = el.parentElement;
      }
      return false;
    };

    const snapBack = function (el) {
      if (!el) return;
      el.style.transition = "transform 0.15s ease-out";
      el.style.transform = "";
      el.classList.remove("swipe-read", "swipe-star", "swipe-open");
      setTimeout(function () { el.style.transition = ""; }, 160);
    };
    // setPreview: reflect WHICH action would fire at the current drag
    // distance (dist = how far left, in px). Rows/articles tint toward
    // read past SHORT and star past LONG; feed rows show the "open" hint.
    const setPreview = function (el, dist) {
      if (kind === "feed") {
        el.classList.toggle("swipe-open", dist >= SHORT);
      } else {
        el.classList.toggle("swipe-star", dist >= LONG);
        el.classList.toggle("swipe-read", dist >= SHORT && dist < LONG);
      }
    };
    const clickSel = function (el, sel) {
      const b = el.querySelector(sel);
      if (b) b.click();
    };
    const reset = function () {
      tracking = false;
      axis = null;
      actionEl = null;
      kind = null;
      letScroll = false;
    };

    document.addEventListener("touchstart", function (e) {
      if (e.touches.length !== 1) { reset(); return; }
      const t = e.touches[0];
      // Never treat a touch inside a form field or the help overlay as a
      // navigation gesture.
      if (inEditable(e) || helpOpen()) { reset(); return; }
      sx = t.clientX; sy = t.clientY;
      tracking = true;
      axis = null;
      actionEl = null; kind = null; letScroll = false;
      const tgt = e.target;
      if (!tgt || !tgt.closest) return;
      // Resolve the left-swipe target. Entry rows take precedence, then
      // feed rows, then the article surface (these live in disjoint DOM
      // regions, so at most one matches in practice).
      const eli = tgt.closest("ul.entries li");
      if (eli && !eli.classList.contains("empty")) { kind = "entry"; actionEl = eli; }
      if (!actionEl) {
        const fli = tgt.closest("ul.feeds li");
        if (fli && !fli.classList.contains("empty")) { kind = "feed"; actionEl = fli; }
      }
      if (!actionEl) {
        const art = tgt.closest(".entry-full");
        // Don't hijack swipes that start on a link/button in the article.
        if (art && !tgt.closest("a, button")) { kind = "article"; actionEl = art; }
      }
      // If the touch began inside a horizontally-scrollable element, let
      // that element scroll — never translate/act on a left swipe.
      if (inHScroll(tgt, actionEl || document.body)) letScroll = true;
    }, { passive: true });

    document.addEventListener("touchmove", function (e) {
      if (!tracking) return;
      if (e.touches.length !== 1) { if (actionEl) snapBack(actionEl); reset(); return; }
      const t = e.touches[0];
      const dx = t.clientX - sx;
      const dy = t.clientY - sy;
      if (axis === null) {
        if (Math.abs(dx) > SLOP && Math.abs(dx) > Math.abs(dy)) axis = "h";
        else if (Math.abs(dy) > SLOP) { axis = "v"; return; }
        else return;
      }
      if (axis !== "h") return;          // vertical — let the page scroll
      if (letScroll) return;             // horizontal scroller — let it scroll
      e.preventDefault();                // committed horizontal swipe
      // Live preview: only the left-swipe (dx < 0) on a captured element
      // translates and previews. The right-swipe (up) is page-level and
      // has no per-element feedback.
      if (actionEl && dx < 0) {
        const tx = Math.max(dx, -CAP);
        actionEl.style.transition = "";
        actionEl.style.transform = "translateX(" + tx + "px)";
        setPreview(actionEl, -dx);
      } else if (actionEl) {
        // Drifted back rightward — clear any preview shown so far.
        actionEl.style.transform = "";
        actionEl.classList.remove("swipe-read", "swipe-star", "swipe-open");
      }
    }, { passive: false });

    document.addEventListener("touchend", function (e) {
      if (!tracking) { reset(); return; }
      const committed = axis === "h" && !letScroll;
      const el = actionEl;
      const k = kind;
      if (committed) {
        const t = (e.changedTouches && e.changedTouches[0]) || null;
        const dx = t ? t.clientX - sx : 0;
        if (el) snapBack(el);
        if (dx >= BACK) {
          // Right swipe → up the hierarchy (page-level).
          goUp();
        } else if (dx < 0 && el) {
          // Left swipe → context action scoped to the captured element.
          const dist = -dx;
          if (k === "feed") {
            if (dist >= SHORT) clickSel(el, "a");
          } else if (k === "article") {
            if (dist >= LONG) clickSel(el, ".actions .starbtn");
            else if (dist >= SHORT) clickSel(el, ".actions .readbtn");
          } else { // entry row
            if (dist >= LONG) clickSel(el, ".starbtn");
            else if (dist >= SHORT) clickSel(el, ".readbtn");
          }
        }
      }
      reset();
    }, { passive: true });

    document.addEventListener("touchcancel", function () {
      if (actionEl) snapBack(actionEl);
      reset();
    }, { passive: true });
  })();

  // ---- mark-all-read click interceptor -----------------------------
  //
  // The server's mark-all-read handler 303-redirects to a hardcoded
  // target ("./?unread=1" for feed scope; "all" for cross-feed). That
  // can't preserve whichever filter / tag / unread state the user had
  // on whichever page they came from before drilling in. Intercept the
  // click here, POST manually, then history.back() so the bfcache
  // reload (see "pageshow" handler above) shows the prior page with
  // fresh state. Falls back to the brand href when there is no useful
  // history (e.g. opened via bookmark, only entry in tab history).
  document.addEventListener("click", function (e) {
    const btn = e.target.closest(".markall");
    if (!btn) return;
    const form = btn.closest("form");
    if (!form) return;
    e.preventDefault();
    const action = form.getAttribute("action") || "";
    // Resolve action against the current document so a relative URL
    // works under any external prefix (e.g. /rss-test/ui/).
    const url = new URL(action, window.location.href).toString();
    btn.disabled = true;
    fetch(url, { method: "POST", credentials: "same-origin" })
      .then(function (r) {
        if (!r.ok) {
          btn.disabled = false;
          window.location.reload();
          return;
        }
        const ref = sameOriginRef();
        if (ref && window.history.length > 1) {
          window.history.back();
        } else {
          window.location.href = BASE;
        }
      })
      .catch(function () {
        btn.disabled = false;
        window.location.reload();
      });
  });

  // ---- htmx swap hooks --------------------------------------------
  //
  // When the split-panel swaps a new entry into #detail-pane, reset
  // the pane's scroll position and re-wire the dwell auto-mark-read
  // behaviour on the newly-inserted article. (kb-focus restoration on
  // list-row swaps is wired separately in the list-view block above,
  // listening directly to htmx:afterSwap + htmx:afterSettle.)
  document.addEventListener("htmx:afterSwap", function (e) {
    const target = e && e.target;
    if (!target) return;
    if (target.id === "detail-pane") {
      // New entry just swapped in — start at the top of the article.
      // Without this the pane keeps its previous scroll position so
      // long entries make the next entry look like it starts mid-body.
      target.scrollTop = 0;
      wireArticle(target.querySelector(".entry-full"));
    }
  });
})();

// ---- collapsible tag groups on the home page ---------------------
// Each .feed-group has a button.feed-group-toggle whose aria-controls
// points at a ul.feeds id. We persist per-tag collapse state in
// localStorage so the user's view sticks across navigations.
(function () {
  "use strict";
  var groups = document.querySelectorAll(".feed-group");
  if (!groups.length) return;
  var KEY = "harb.collapsedTags";
  var collapsed = {};
  try {
    var raw = localStorage.getItem(KEY);
    if (raw) collapsed = JSON.parse(raw) || {};
  } catch (e) { collapsed = {}; }
  function save() {
    try { localStorage.setItem(KEY, JSON.stringify(collapsed)); } catch (e) {}
  }
  groups.forEach(function (g) {
    var tag = g.dataset.tag || "";
    var btn = g.querySelector(".feed-group-toggle");
    var ul = g.querySelector("ul.feeds");
    if (!btn || !ul) return;
    function apply(isOpen) {
      btn.setAttribute("aria-expanded", isOpen ? "true" : "false");
      if (isOpen) ul.removeAttribute("hidden");
      else ul.setAttribute("hidden", "");
    }
    apply(!collapsed[tag]);
    btn.addEventListener("click", function () {
      var nowOpen = btn.getAttribute("aria-expanded") !== "true";
      apply(nowOpen);
      if (nowOpen) delete collapsed[tag]; else collapsed[tag] = true;
      save();
    });
  });
})();


/* copy-to-clipboard buttons (e.g. RSS feed URL) */
(function () {
  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest && ev.target.closest(".copy-url");
    if (!btn) return;
    ev.preventDefault();
    var sel = btn.getAttribute("data-copy-target");
    var el = sel && document.querySelector(sel);
    if (!el) return;
    var text = el.value != null ? el.value : el.textContent;
    function done() {
      var prev = btn.textContent;
      btn.textContent = "copied";
      btn.classList.add("copied");
      setTimeout(function () { btn.textContent = prev; btn.classList.remove("copied"); }, 1200);
    }
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done, function () { el.focus(); el.select(); });
    } else {
      el.focus(); el.select();
      try { document.execCommand("copy"); done(); } catch (e) {}
    }
  });
})();

// Token-aware tag autocomplete for comma-separated tag inputs
// (the add-feed page). Progressive enhancement on top of a plain
// <datalist>: with JS off the datalist still completes the whole field
// (useful for the first tag); with JS on we rewrite the <option>
// values on every keystroke so the browser's native datalist completes
// only the *current* token — the text after the last comma — while
// preserving the tags already typed.
//
//   input "tech, da"  + candidate "daily"
//     → option value "tech, daily"  (native datalist offers "daily")
//
// Candidates already present in the field are dropped from the list.
(function () {
  function tokens(s) {
    return s.split(",").map(function (t) { return t.trim(); });
  }
  function enhance(input) {
    var list = document.getElementById(input.getAttribute("list"));
    if (!list) return;
    // Snapshot the original single-tag candidate values once.
    var base = Array.prototype.map.call(
      list.querySelectorAll("option"),
      function (o) { return o.value; }
    );
    function rewrite() {
      var val = input.value;
      var lastComma = val.lastIndexOf(",");
      var prefix = lastComma < 0 ? "" : val.slice(0, lastComma + 1);
      var token = val.slice(lastComma + 1).trim().toLowerCase();
      var chosen = tokens(val.slice(0, lastComma < 0 ? 0 : lastComma))
        .filter(Boolean)
        .map(function (t) { return t.toLowerCase(); });
      var sep = prefix && !/\s$/.test(prefix) ? prefix + " " : prefix;
      list.innerHTML = "";
      base.forEach(function (cand) {
        var lc = cand.toLowerCase();
        if (chosen.indexOf(lc) !== -1) return; // already typed
        if (token && lc.indexOf(token) !== 0) return; // prefix filter
        var opt = document.createElement("option");
        opt.value = sep + cand;
        list.appendChild(opt);
      });
    }
    input.addEventListener("input", rewrite);
  }
  function init() {
    var inputs = document.querySelectorAll("input[data-tag-autocomplete]");
    Array.prototype.forEach.call(inputs, enhance);
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
/* auto-refresh poll — surface new entries without a manual reload.
 *
 * On authenticated pages the server stamps <html data-state-ver="...">
 * with Store.StateVersion() at render time and exposes GET /ui/version
 * (the same value, body + ETag). We poll it on a single interval with
 * If-None-Match pinned to the page-load value and never advanced: the
 * server answers 304 while nothing has changed, and 200 (with a new
 * value) the moment new entries land or read/star state mutates. On
 * that first 200 we latch an unobtrusive "new items" pill that reloads
 * on tap — we never auto-swap the list, which would jump scroll
 * position and clobber keyboard selection mid-read. Polling pauses
 * while the tab is hidden and resumes on visibilitychange, so a
 * backgrounded tab costs nothing. */
(function () {
  var root = document.documentElement;
  var pageVer = root.getAttribute("data-state-ver");
  // Only authenticated pages carry the attribute (and the pill).
  if (pageVer == null) return;
  var pill = document.getElementById("refresh-pill");
  if (!pill) return;

  var BASE = root.dataset.uiBase || "./";
  var URL_ = BASE + "version";
  var INM = '"' + pageVer + '"';
  var PERIOD = 50000; // ~50s between polls
  var timer = null;
  var shown = false;
  var dismissed = false;

  function show() {
    if (shown || dismissed) return;
    shown = true;
    pill.hidden = false;
    stop(); // nothing more to learn — release the interval
  }

  function poll() {
    if (shown) return; // nothing more to learn once latched
    fetch(URL_, {
      headers: { "If-None-Match": INM },
      cache: "no-store",
      credentials: "same-origin"
    }).then(function (res) {
      // 304 → unchanged. Anything other than a clean 200 (redirects to
      // login, errors) is ignored so a stray body can't false-trigger.
      if (res.status !== 200) return;
      return res.text().then(function (body) {
        if (body && body.trim() !== pageVer) show();
      });
    }).catch(function () { /* transient network error — try again next tick */ });
  }

  function start() {
    if (timer != null || shown) return;
    timer = setInterval(poll, PERIOD);
  }
  function stop() {
    if (timer != null) { clearInterval(timer); timer = null; }
  }

  document.addEventListener("visibilitychange", function () {
    if (document.hidden) stop();
    else { poll(); start(); }
  });

  var btn = pill.querySelector(".refresh-pill-btn");
  if (btn) btn.addEventListener("click", function () { window.location.reload(); });
  var x = pill.querySelector(".refresh-pill-x");
  if (x) x.addEventListener("click", function () {
    dismissed = true;
    pill.hidden = true;
    stop();
  });

  if (!document.hidden) start();
})();
