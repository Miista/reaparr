const SERVICES = [
  { id: 'jellyfin', name: 'Jellyfin', hue: 290, required: true, desc: 'Reaparr needs Jellyfin for watch state and the activity log.' },
  { id: 'seerr', name: 'Seerr', hue: 150, required: true, desc: 'After deletions, Reaparr cleans up the stale requests Seerr leaves behind for media that no longer exists.' },
  { id: 'radarr', name: 'Radarr', hue: 60, required: false, desc: 'Leave blank for a TV-only deployment. At least one of Radarr/Sonarr must be configured.' },
  { id: 'sonarr', name: 'Sonarr', hue: 200, required: false, desc: 'Leave blank for a movie-only deployment. At least one of Radarr/Sonarr must be configured.' },
];

const SETTINGS_KEYS = ['movies_grace_period', 'tv_grace_period', 'poll_schedule', 'log_level', 'daemon_enabled', 'keep_tag'];
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
const pad = (n) => String(n).padStart(2, '0');

// MIN_GRACE_SECONDS mirrors settings.MinGracePeriod (1 day).
const MIN_GRACE_SECONDS = 86400;

// parseDuration mirrors settings.ParseGracePeriod: "<n>d", "<n>w", or a
// Go duration string (e.g. 36h, 1h30m). Returns seconds, or null if invalid.
function parseDuration(raw) {
  const v = (raw || '').trim();
  let m = /^(\d+(?:\.\d+)?)([dw])$/.exec(v);
  if (m) return +m[1] * (m[2] === 'd' ? 86400 : 604800);
  if (!/^(?:\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h))+$/.test(v)) return null;
  const units = { ns: 1e-9, us: 1e-6, 'µs': 1e-6, ms: 1e-3, s: 1, m: 60, h: 3600 };
  let total = 0;
  for (const [, n, u] of v.matchAll(/(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g)) total += +n * units[u];
  return total;
}

function plural(n, unit) {
  const r = Math.round(n * 10) / 10;
  return `${r} ${unit}${r === 1 ? '' : 's'}`;
}

// humanCountdown rounds to whole units for "time left" displays: days from
// two days up, otherwise whole hours, then minutes.
function humanCountdown(s) {
  if (s >= 2 * 86400) return plural(Math.round(s / 86400), 'day');
  if (s >= 3600) return plural(Math.floor(s / 3600), 'hour');
  return plural(Math.max(1, Math.floor(s / 60)), 'minute');
}

function humanSeconds(s) {
  if (s >= 86400) return plural(s / 86400, 'day');
  if (s >= 3600) return plural(s / 3600, 'hour');
  return plural(s / 60, 'minute');
}

function App() {
  return {
    tab: 'due',
    settingsTab: 'general', // 'general' | 'connections'
    services: SERVICES,
    now: Date.now(),

    due: [],
    dueLoading: false,
    dueError: '',
    deletingID: null,
    deletingSelected: false,
    selected: [], // ids of selected rows
    keepingSelected: false,

    library: [], // every Radarr movie / Sonarr series: { service, id, title, year, kind, kept }
    libraryLoading: false,
    libraryError: '',
    libraryQuery: '',
    libraryFilter: 'all',
    libraryFilters: [
      { key: 'all', label: 'All' },
      { key: 'kept', label: 'Kept' },
      { key: 'movie', label: 'Movies' },
      { key: 'series', label: 'Series' },
    ],
    libraryBusy: false,
    deleteResult: '',

    missingServices: [],
    daemonEnabled: true,
    nextRun: null,

    settings: { values: {}, env_managed: {} },
    savedSettings: {},
    settingsSavedAt: null,
    settingsError: '',
    durationFields: [
      { key: 'movies_grace_period', label: 'Delete movies after', help: 'How long after a movie is fully watched before it is deleted.' },
      { key: 'tv_grace_period', label: 'Delete TV seasons after', help: 'How long after the last episode of a fully watched season stops playing before the season is deleted.' },
    ],

    connections: { values: { jellyfin: {}, radarr: {}, sonarr: {}, seerr: {} }, env_managed: {} },
    savedUrls: {},
    connectionKeyInputs: { jellyfin: '', radarr: '', sonarr: '', seerr: '' },
    connStatus: {}, // service -> { status: idle|testing|ok|fail, msg }
    connectionsSavedAt: null,
    connectionsError: '',

    async mounted() {
      this.applyRoute(true);
      window.addEventListener('hashchange', () => this.applyRoute());
      setInterval(() => { this.now = Date.now(); }, 30000);
      document.addEventListener('keydown', (e) => this.onDialogKey(e));
      await Promise.all([this.loadStatus(), this.loadDue(), this.loadSettings(), this.loadConnections(), this.loadLibrary()]);
      this.testConfiguredConnections();
      // Only now — with status and connections loaded and the tests started
      // (they count as pending until done) — can problems be judged.
      this.loaded = true;
    },

    // --- Status ---------------------------------------------------------

    async loadStatus() {
      const res = await fetch('/api/status');
      if (!res.ok) return;
      const data = await res.json();
      this.missingServices = data.missing_services || [];
      this.daemonEnabled = data.daemon_enabled !== false;
      this.nextRun = data.next_run || null;
    },

    // requirements combines "configured" (backend) with the connection tests
    // (frontend): a requirement is done only once one of its services has
    // passed its test. While a test is still running it counts as pending,
    // so a page load doesn't flash false alarms.
    get requirements() {
      const groups = [
        { name: 'Jellyfin', ids: ['jellyfin'] },
        { name: 'Radarr or Sonarr', ids: ['radarr', 'sonarr'] },
        { name: 'Seerr', ids: ['seerr'] },
      ];
      return groups.map((g) => {
        const states = g.ids.map((id) => (this.isConfigured(id) ? this.connState(id).status : 'missing'));
        if (states.includes('ok')) return { ...g, state: 'ok' };
        if (states.includes('testing') || states.includes('idle')) return { ...g, state: 'pending' };
        if (states.includes('fail')) return { ...g, state: 'fail' };
        return { ...g, state: 'missing' };
      });
    },

    get problems() {
      return this.requirements.filter((r) => r.state === 'fail' || r.state === 'missing');
    },

    get doneCount() {
      return this.requirements.filter((r) => r.state === 'ok').length;
    },

    loaded: false, // initial status/connections load finished

    // showProblems gates the banner, alert dots and problem badge until the
    // first load has settled, so they don't flash on page load.
    get showProblems() {
      return this.loaded && (this.missingServices.length > 0 || this.problems.length > 0);
    },

    get runState() {
      if (!this.loaded) return { label: 'Checking…', cls: 'checking' };
      if (this.missingServices.length) return { label: 'Not running', cls: 'stopped' };
      if (this.problems.length) return { label: 'Connection problem', cls: 'stopped' };
      if (!this.daemonEnabled) return { label: 'Manual only', cls: 'manual' };
      return { label: 'Scheduled', cls: 'running' };
    },

    // --- Due for deletion -----------------------------------------------

    // Every listed item can be deleted by hand right away; the grace period
    // (item.due) only decides when the scheduled sweep deletes it.
    get resolvedCount() {
      return this.due.filter((item) => item.resolved).length;
    },

    isSelected(item) {
      return this.selected.includes(item.id);
    },

    toggleSelect(item) {
      this.selected = this.isSelected(item) ? this.selected.filter((id) => id !== item.id) : [...this.selected, item.id];
    },

    get allSelected() {
      return this.resolvedCount > 0 && this.selected.length === this.resolvedCount;
    },

    get someSelected() {
      return this.selected.length > 0;
    },

    toggleAll() {
      this.selected = this.allSelected ? [] : this.due.filter((item) => item.resolved).map((item) => item.id);
    },

    // fresh=true rebuilds the list from live Jellyfin/Radarr/Sonarr data
    // (the Refresh button); otherwise the server's cached preview is shown.
    async loadDue(fresh = false) {
      if (fresh) this.selected = [];
      this.dueLoading = true;
      this.dueError = '';
      try {
        const res = await fetch(fresh ? '/api/due?fresh=1' : '/api/due');
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          throw new Error(data.error || 'Failed to load due items.');
        }
        const data = await res.json();
        // Soonest first: overdue items lead, then the waiting ones by when
        // their grace period ends.
        this.due = (data.due || []).sort((a, b) => new Date(a.due_at) - new Date(b.due_at));
        // Drop selections for rows that are gone or no longer deletable.
        const deletable = new Set(this.due.filter((item) => item.resolved).map((item) => item.id));
        this.selected = this.selected.filter((id) => deletable.has(id));
      } catch (err) {
        this.dueError = err.message;
      } finally {
        this.dueLoading = false;
      }
    },

    // deleteSelected deletes the selected rows (each re-checked live first).
    async deleteSelected() {
      const ids = [...this.selected];
      if (!await this.ask({
        title: `Delete ${ids.length} selected item${ids.length === 1 ? '' : 's'}?`,
        message: 'Each is re-checked first; only what still qualifies is deleted. Files are removed through Radarr/Sonarr.',
        confirmLabel: `Delete ${ids.length}`,
        tone: 'danger',
      })) return;

      this.deletingSelected = true;
      this.dueError = '';
      this.deleteResult = '';
      try {
        const res = await fetch('/api/due/delete-selected', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ ids }),
        });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(data.error || 'Delete failed.');
        this.deleteResult = `Deleted ${data.deleted}, skipped ${data.skipped}, failed ${data.failed}.`;
        this.selected = [];
        await this.loadDue();
      } catch (err) {
        this.dueError = err.message;
      } finally {
        this.deletingSelected = false;
      }
    },

    async keepItem(item) {
      if (item.kind === 'season') {
        const series = item.title.split(' — ')[0];
        if (!await this.ask({
          title: `Keep ${series}?`,
          message: 'This keeps the whole series in Sonarr — every season, not just this one.',
          confirmLabel: 'Keep series',
          tone: 'keep',
        })) return;
      }
      await this.keepDueRows([item]);
    },

    // keepDueRows keeps the movies/series behind Due rows. Keeping can't
    // delete anything, so there's no live re-check: the rows (and any other
    // seasons of a kept series) leave the list immediately, and the tag is
    // set directly by Radarr/Sonarr ID — the same fast path as the Library.
    async keepDueRows(rows) {
      const refs = [];
      const seen = new Set();
      for (const r of rows) {
        const key = `${r.arr_service}:${r.arr_id}`;
        if (r.resolved && r.arr_service && !seen.has(key)) {
          seen.add(key);
          refs.push({ service: r.arr_service, id: r.arr_id });
        }
      }
      if (!refs.length) return;

      this.due = this.due.filter((d) => !seen.has(`${d.arr_service}:${d.arr_id}`));
      this.selected = this.selected.filter((id) => this.due.some((d) => d.id === id));
      for (const it of this.library) {
        if (seen.has(`${it.service}:${it.id}`)) it.kept = true;
      }

      this.dueError = '';
      this.deleteResult = '';
      try {
        const res = await fetch('/api/library/keep', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ keep: true, items: refs }),
        });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(data.error || 'Keep failed.');
      } catch (err) {
        this.dueError = err.message;
        await Promise.all([this.loadDue(), this.loadLibrary()]);
      }
    },

    // --- Confirmation dialog --------------------------------------------

    dialog: null, // { title, message, confirmLabel, tone, resolve }

    // ask shows the styled confirmation dialog and resolves to true/false.
    ask(opts) {
      return new Promise((resolve) => {
        this.dialog = { ...opts, resolve };
        setTimeout(() => document.querySelector('.dialog .btn-confirm')?.focus());
      });
    },

    closeDialog(answer) {
      const d = this.dialog;
      this.dialog = null;
      d?.resolve(answer);
    },

    // Escape cancels. Enter needs no handler: the confirm button is focused
    // when the dialog opens, so Enter activates whichever button has focus.
    onDialogKey(e) {
      if (this.dialog && e.key === 'Escape') this.closeDialog(false);
    },

    // keepSelected marks every selected row as a keeper. Seasons keep their
    // whole series, so the dialog says so when any are selected.
    async keepSelected() {
      const ids = [...this.selected];
      const rows = this.due.filter((item) => ids.includes(item.id));
      const seasons = rows.filter((item) => item.kind === 'season').length;
      if (!await this.ask({
        title: `Keep ${ids.length} selected item${ids.length === 1 ? '' : 's'}?`,
        message: seasons
          ? 'They will never be deleted. Selected seasons keep their whole series in Sonarr — every season.'
          : 'They will never be deleted, not on schedule and not by hand.',
        confirmLabel: `Keep ${ids.length}`,
        tone: 'keep',
      })) return;

      this.keepingSelected = true;
      try {
        await this.keepDueRows(rows);
      } finally {
        this.keepingSelected = false;
      }
    },

    // --- Library --------------------------------------------------------

    libKey(it) {
      return `${it.service}:${it.id}`;
    },

    get filteredLibrary() {
      const q = this.libraryQuery.trim().toLowerCase();
      return this.library.filter((it) => {
        if (this.libraryFilter === 'kept' && !it.kept) return false;
        if ((this.libraryFilter === 'movie' || this.libraryFilter === 'series') && it.kind !== this.libraryFilter) return false;
        return !q || it.title.toLowerCase().includes(q);
      });
    },

    // --- Routing ----------------------------------------------------------
    // The current page lives in the URL hash (#/due, #/library,
    // #/settings/general, #/settings/connections), so a refresh, a
    // bookmark or the back button lands on the same page.

    applyRoute(initial = false) {
      const [tab, sub] = location.hash.replace(/^#\/?/, '').split('/');
      if (tab === 'library' || tab === 'settings') {
        this.tab = tab;
      } else {
        this.tab = 'due';
      }
      if (tab === 'settings') {
        this.settingsTab = sub === 'connections' ? 'connections' : 'general';
      }
      // Entering the Library re-reads it live (mounted already loads it once).
      if (this.tab === 'library' && !initial) this.loadLibrary();
    },

    async loadLibrary() {
      this.libraryLoading = true;
      this.libraryError = '';
      try {
        const res = await fetch('/api/library');
        const data = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(data.error || 'Failed to load the library.');
        this.library = (data.items || []).sort((a, b) => a.title.localeCompare(b.title));
      } catch (err) {
        this.libraryError = err.message;
      } finally {
        this.libraryLoading = false;
      }
    },

    // One click either way — except unkeeping something the scheduled run
    // would then delete: watched and past its grace period ("due"), or
    // unknown because Jellyfin couldn't be checked. With the daemon off
    // nothing is deleted automatically, so no confirmation is needed.
    async toggleKeep(it) {
      if (it.kept && this.daemonEnabled && (it.watch === 'due' || !it.watch)) {
        const when = this.nextRun
          ? `in ${humanCountdown(Math.max(60, (new Date(this.nextRun) - Date.now()) / 1000))}`
          : 'soon';
        const ok = await this.ask({
          title: `Unkeep ${it.title}?`,
          message: it.watch === 'due'
            ? `It's watched and past its grace period, so the next scheduled run (${when}) will delete it.`
            : `Reaparr couldn't check whether it's watched. If it is and its grace period has passed, the next scheduled run (${when}) will delete it.`,
          confirmLabel: 'Unkeep',
          tone: 'danger',
        });
        if (!ok) return;
      }
      await this.setKept([it], !it.kept);
    },

    async setKept(items, keep) {
      // Optimistic: flip the toggles and, when keeping, drop the matching
      // Due rows (a series takes all its seasons) right away.
      const refs = new Set(items.map((it) => `${it.service}:${it.id}`));
      const before = items.map((it) => it.kept);
      for (const it of items) it.kept = keep;
      if (keep) {
        this.due = this.due.filter((d) => !refs.has(`${d.arr_service}:${d.arr_id}`));
        this.selected = this.selected.filter((id) => this.due.some((d) => d.id === id));
      }

      this.libraryBusy = true;
      this.libraryError = '';
      try {
        const res = await fetch('/api/library/keep', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ keep, items: items.map((it) => ({ service: it.service, id: it.id })) }),
        });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(data.error || 'Update failed.');
        // The backend has already updated its due list before responding.
        await this.loadDue();
      } catch (err) {
        items.forEach((it, i) => { it.kept = before[i]; });
        this.libraryError = err.message;
        await Promise.all([this.loadLibrary(), this.loadDue()]);
      } finally {
        this.libraryBusy = false;
      }
    },

    async deleteItem(item) {
      if (!await this.ask({
        title: `Delete ${item.title}?`,
        message: 'It is re-checked first, then its files are removed through Radarr/Sonarr.',
        confirmLabel: 'Delete',
        tone: 'danger',
      })) return;

      this.deletingID = item.id;
      this.dueError = '';
      try {
        const res = await fetch('/api/due/delete', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ id: item.id }),
        });
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          throw new Error(data.error || 'Delete failed.');
        }
        await this.loadDue();
      } catch (err) {
        this.dueError = err.message;
      } finally {
        this.deletingID = null;
      }
    },

    hueFor(title) {
      const hues = [290, 150, 60, 200, 25, 330];
      let h = 0;
      for (const c of title || '') h = (h * 31 + c.charCodeAt(0)) >>> 0;
      return hues[h % hues.length];
    },

    formatDate(d) {
      return `${d.getDate()} ${MONTHS[d.getMonth()]}`;
    },

    formatDateTime(iso) {
      if (!iso) return '—';
      const d = new Date(iso);
      return `${d.getDate()} ${MONTHS[d.getMonth()]} ${d.getFullYear()}, ${pad(d.getHours())}:${pad(d.getMinutes())}`;
    },

    ago(iso) {
      const secs = (this.now - new Date(iso)) / 1000;
      if (secs < 3600) return 'Just now';
      if (secs < 86400) return plural(Math.floor(secs / 3600), 'hour') + ' ago';
      const days = Math.floor(secs / 86400);
      return days === 1 ? 'Yesterday' : `${days} days ago`;
    },

    // deletionWhen describes when a row will actually be deleted: after its
    // grace period, and then only on the next scheduled sweep (if the daemon
    // is enabled).
    deletionWhen(item) {
      if (!item.resolved) return { main: "Can't be deleted", sub: item.reason || '', cls: 'when-blocked' };
      const due = new Date(item.due_at);
      if (due > this.now) {
        return { main: humanCountdown((due - this.now) / 1000), sub: `${this.formatDate(due)}, ${pad(due.getHours())}:${pad(due.getMinutes())}`, cls: '' };
      }
      if (this.daemonEnabled && this.nextRun) {
        const secs = Math.max(0, (new Date(this.nextRun) - this.now) / 1000);
        return { main: secs < 60 ? 'Under a minute' : humanCountdown(secs), sub: `Next scheduled run · due since ${this.formatDate(due)}`, cls: 'when-scheduled' };
      }
      return { main: 'Not scheduled', sub: `Auto-delete is off · due since ${this.formatDate(due)}`, cls: 'when-off' };
    },

    // flashSaved shows a save bar's green "… saved" message for a few
    // seconds, then lets it fall back to "All changes saved".
    flashSaved(key) {
      const at = Date.now();
      this[key] = at;
      setTimeout(() => { if (this[key] === at) this[key] = null; }, 3000);
    },

    // --- Settings -------------------------------------------------------

    parseDuration,

    // graceValid: parses, and is at least the 1 day minimum.
    graceValid(raw) {
      const secs = parseDuration((raw || '').trim());
      return secs !== null && secs >= MIN_GRACE_SECONDS;
    },

    // durationHuman is the "= 7 days" shown beside a valid value ('' when
    // invalid — durationError explains instead).
    durationHuman(raw) {
      const v = (raw || '').trim();
      if (!this.graceValid(v)) return '';
      const w = /^(\d+(?:\.\d+)?)w$/.exec(v);
      return '= ' + (w ? plural(+w[1], 'week') : humanSeconds(parseDuration(v)));
    },

    // durationError explains an invalid grace period ('' when valid). Shown
    // on its own line below the input, so it never fights the input for
    // space.
    durationError(raw) {
      const v = (raw || '').trim();
      const secs = parseDuration(v);
      if (secs === null) {
        // Days/weeks can't be combined with other units (e.g. 7d2h) — say
        // so, and offer the exact equivalent without days/weeks.
        const units = { w: 604800, d: 86400, h: 3600, m: 60, s: 1 };
        const parts = [...v.matchAll(/(\d+(?:\.\d+)?)([wdhms])/g)];
        const isCompound = parts.length > 1 && parts.map((p) => p[0]).join('') === v && /[dw]/.test(v);
        if (isCompound) {
          const total = Math.round(parts.reduce((sum, p) => sum + +p[1] * units[p[2]], 0));
          const h = Math.floor(total / 3600);
          const m = Math.floor((total % 3600) / 60);
          const s = total % 60;
          return `Can't mix days/weeks with other units — use ${h}h${m ? m + 'm' : ''}${s ? s + 's' : ''}`;
        }
        return 'Use a number + h, d or w (e.g. 7d, 36h, 2w)';
      }
      if (secs < MIN_GRACE_SECONDS) return 'Minimum is 1 day';
      return '';
    },

    cronHuman(raw) {
      const descriptors = {
        '@hourly': 'Every hour', '@daily': 'Every day at midnight', '@midnight': 'Every day at midnight',
        '@weekly': 'Every Sunday', '@monthly': 'First of each month', '@yearly': 'Once a year', '@annually': 'Once a year',
      };
      const v = (raw || '').trim();
      if (descriptors[v]) return descriptors[v];
      if (/^@every\s+\S+$/.test(v)) return 'Every ' + v.split(/\s+/)[1];
      return v.split(/\s+/).length === 5 ? 'Custom schedule' : null;
    },

    snapshotSettings() {
      const s = {};
      for (const k of SETTINGS_KEYS) s[k] = this.settings.values[k];
      return s;
    },

    get settingsDirty() {
      return SETTINGS_KEYS.some((k) => this.settings.values[k] !== this.savedSettings[k]);
    },

    get settingsValid() {
      const managed = this.settings.env_managed;
      const v = this.settings.values;
      return (managed.movies_grace_period || this.graceValid(v.movies_grace_period))
        && (managed.tv_grace_period || this.graceValid(v.tv_grace_period))
        && (managed.poll_schedule || this.cronHuman(v.poll_schedule) !== null)
        && (managed.keep_tag || !!(v.keep_tag || '').trim());
    },

    get settingsBar() {
      if (this.settingsError) return { msg: this.settingsError, cls: 'failed' };
      if (!this.settingsValid) return { msg: 'Fix the highlighted fields before saving', cls: 'invalid' };
      if (this.settingsDirty) return { msg: 'You have unsaved changes', cls: 'dirty' };
      if (this.settingsSavedAt) return { msg: 'Settings saved', cls: 'saved' };
      return { msg: 'All changes saved', cls: '' };
    },

    async loadSettings() {
      const res = await fetch('/api/settings');
      if (!res.ok) return;
      this.settings = await res.json();
      this.savedSettings = this.snapshotSettings();
    },

    discardSettings() {
      Object.assign(this.settings.values, this.savedSettings);
      this.settingsError = '';
    },

    async saveSettings() {
      if (!this.settingsValid) return;
      this.settingsError = '';
      try {
        const res = await fetch('/api/settings', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(this.settings.values),
        });
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          throw new Error(data.error || 'Failed to save settings.');
        }
        this.settings = await res.json();
        this.savedSettings = this.snapshotSettings();
        this.flashSaved('settingsSavedAt');
        await this.loadStatus();
      } catch (err) {
        this.settingsError = err.message;
      }
    },

    // --- Connections ----------------------------------------------------

    connState(id) {
      return this.connStatus[id] || { status: 'idle', msg: '' };
    },

    isConfigured(id) {
      const v = this.connections.values[id] || {};
      return !!v.url && (!!v.api_key_set || !!this.connectionKeyInputs[id]);
    },

    connPillLabel(id) {
      const s = this.connState(id).status;
      if (s === 'testing') return 'Testing';
      if (s === 'ok') return 'Connected';
      if (s === 'fail') return 'Failed';
      return this.isConfigured(id) ? 'Not tested' : 'Not configured';
    },

    connMessage(id) {
      const st = this.connState(id);
      if (st.msg) return st.msg;
      return this.isConfigured(id) ? 'Ready to test.' : 'Add an API key to test.';
    },

    connEdited(id) {
      this.connStatus = { ...this.connStatus, [id]: { status: 'idle', msg: '' } };
      this.connectionsSavedAt = null;
    },

    get connectionsDirty() {
      return SERVICES.some((s) => (this.connections.values[s.id] || {}).url !== this.savedUrls[s.id] || !!this.connectionKeyInputs[s.id]);
    },

    get connectionsBar() {
      if (this.connectionsError) return { msg: this.connectionsError, cls: 'failed' };
      if (this.connectionsDirty) return { msg: 'You have unsaved changes', cls: 'dirty' };
      if (this.connectionsSavedAt) return { msg: 'Connections saved', cls: 'saved' };
      return { msg: 'All changes saved', cls: '' };
    },

    applyConnections(data) {
      this.connections = data;
      this.savedUrls = Object.fromEntries(SERVICES.map((s) => [s.id, (data.values[s.id] || {}).url]));
    },

    async loadConnections() {
      const res = await fetch('/api/connections');
      if (!res.ok) return;
      this.applyConnections(await res.json());
    },

    // testConfiguredConnections runs "Test connection" for every service
    // that has both a URL and a saved key, so the cards reflect reality on
    // load rather than waiting for a click.
    testConfiguredConnections() {
      for (const s of SERVICES) {
        if (this.isConfigured(s.id)) this.testConnection(s.id);
      }
    },

    discardConnections() {
      for (const s of SERVICES) this.connections.values[s.id].url = this.savedUrls[s.id];
      this.connectionKeyInputs = { jellyfin: '', radarr: '', sonarr: '', seerr: '' };
      this.connectionsError = '';
      this.connStatus = {};
      this.testConfiguredConnections();
    },

    async saveConnections() {
      this.connectionsError = '';
      try {
        const payload = {};
        for (const s of SERVICES) {
          payload[s.id] = {
            url: this.connections.values[s.id].url,
            api_key: this.connectionKeyInputs[s.id] || '',
          };
        }
        const res = await fetch('/api/connections', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload),
        });
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          throw new Error(data.error || 'Failed to save connections.');
        }
        this.applyConnections(await res.json());
        this.connectionKeyInputs = { jellyfin: '', radarr: '', sonarr: '', seerr: '' };
        this.flashSaved('connectionsSavedAt');
        this.connStatus = {};
        await this.loadStatus();
        this.testConfiguredConnections();
      } catch (err) {
        this.connectionsError = err.message;
      }
    },

    async testConnection(id) {
      const started = performance.now();
      this.connStatus = { ...this.connStatus, [id]: { status: 'testing', msg: `Reaching ${this.connections.values[id].url}…` } };
      try {
        const res = await fetch('/api/connections/test', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            service: id,
            url: this.connections.values[id].url,
            api_key: this.connectionKeyInputs[id] || '',
          }),
        });
        const data = await res.json();
        const ms = Math.round(performance.now() - started);
        this.connStatus = {
          ...this.connStatus,
          [id]: data.ok ? { status: 'ok', msg: `Connected · responded in ${ms} ms` } : { status: 'fail', msg: data.error || 'Connection failed.' },
        };
      } catch (err) {
        this.connStatus = { ...this.connStatus, [id]: { status: 'fail', msg: err.message } };
      }
    },
  };
}

PetiteVue.createApp({ App }).mount('#app');
