const SERVICES = [
  { id: 'jellyfin', name: 'Jellyfin', hue: 290, required: true, desc: 'Reaparr needs Jellyfin for watch state and the activity log.' },
  { id: 'seerr', name: 'Seerr', hue: 150, required: true, desc: 'After deletions, Reaparr cleans up the stale requests Seerr leaves behind for media that no longer exists.' },
  { id: 'radarr', name: 'Radarr', hue: 60, required: false, desc: 'Leave blank for a TV-only deployment. At least one of Radarr/Sonarr must be configured.' },
  { id: 'sonarr', name: 'Sonarr', hue: 200, required: false, desc: 'Leave blank for a movie-only deployment. At least one of Radarr/Sonarr must be configured.' },
];

const SETTINGS_KEYS = ['movies_grace_period', 'tv_grace_period', 'poll_schedule', 'log_level', 'daemon_enabled'];
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
const pad = (n) => String(n).padStart(2, '0');

// parseDuration mirrors duration.go's parseGracePeriod: "<n>d", "<n>w", or a
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
    services: SERVICES,
    now: Date.now(),

    due: [],
    dueLoading: false,
    dueError: '',
    deletingID: null,
    deletingSelected: false,
    selected: [], // ids of selected rows
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
      setInterval(() => { this.now = Date.now(); }, 30000);
      await Promise.all([this.loadStatus(), this.loadDue(), this.loadSettings(), this.loadConnections()]);
      this.testConfiguredConnections();
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

    get runState() {
      if (this.missingServices.length) return { label: 'Not running', cls: 'stopped' };
      if (this.problems.length) return { label: 'Connection problem', cls: 'stopped' };
      if (!this.daemonEnabled) return { label: 'Manual only', cls: 'manual' };
      return { label: 'Running', cls: 'running' };
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
      if (!confirm(`Delete ${ids.length} selected item(s) now? Each is re-checked first; only what still qualifies is deleted.`)) return;

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

    async deleteItem(item) {
      if (!confirm(`Delete '${item.title}' now? It is re-checked first.`)) return;

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

    // --- Settings -------------------------------------------------------

    parseDuration,

    durationHuman(raw) {
      const v = (raw || '').trim();
      const secs = parseDuration(v);
      if (secs === null) return 'Use a number + h, d or w';
      const w = /^(\d+(?:\.\d+)?)w$/.exec(v);
      return '= ' + (w ? plural(+w[1], 'week') : humanSeconds(secs));
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
      return (managed.movies_grace_period || parseDuration(v.movies_grace_period) !== null)
        && (managed.tv_grace_period || parseDuration(v.tv_grace_period) !== null)
        && (managed.poll_schedule || this.cronHuman(v.poll_schedule) !== null);
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
        this.settingsSavedAt = Date.now();
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
        this.connectionsSavedAt = Date.now();
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
