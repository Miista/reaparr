function App() {
  return {
    tab: 'due',

    due: [],
    dueLoading: false,
    dueError: '',
    deletingID: null,

    settings: { values: {}, env_managed: {} },
    settingsError: '',
    settingsSaved: false,

    connections: { values: { jellyfin: {}, radarr: {}, sonarr: {}, seerr: {} }, env_managed: {} },
    connectionKeyInputs: { jellyfin: '', radarr: '', sonarr: '', seerr: '' },
    connectionsError: '',
    connectionsSaved: false,
    testingService: null,
    testResults: {},

    missingServices: [],
    daemonEnabled: true,
    deletingAll: false,
    deleteAllResult: '',

    async mounted() {
      await Promise.all([this.loadStatus(), this.loadDue(), this.loadSettings(), this.loadConnections()]);
    },

    async loadStatus() {
      const res = await fetch('/api/status');
      if (!res.ok) return;
      const data = await res.json();
      this.missingServices = data.missing_services || [];
      this.daemonEnabled = data.daemon_enabled !== false;
    },

    async deleteAll() {
      const n = this.due.filter((item) => item.resolved).length;
      if (!confirm(`Delete ${n} item(s) now? Everything is re-checked first; only what still qualifies is deleted.`)) return;

      this.deletingAll = true;
      this.dueError = '';
      this.deleteAllResult = '';
      try {
        const res = await fetch('/api/due/delete-all', { method: 'POST' });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(data.error || 'Delete all failed.');
        this.deleteAllResult = `Deleted ${data.deleted}, skipped ${data.skipped}, failed ${data.failed}.`;
        await this.loadDue();
      } catch (err) {
        this.dueError = err.message;
      } finally {
        this.deletingAll = false;
      }
    },

    // fresh=true rebuilds the list from live Jellyfin/Radarr/Sonarr data
    // (the Refresh button); otherwise the server's cached preview is shown.
    async loadDue(fresh = false) {
      this.dueLoading = true;
      this.dueError = '';
      try {
        const res = await fetch(fresh ? '/api/due?fresh=1' : '/api/due');
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          throw new Error(data.error || 'Failed to load due items.');
        }
        const data = await res.json();
        this.due = data.due || [];
      } catch (err) {
        this.dueError = err.message;
      } finally {
        this.dueLoading = false;
      }
    },

    async deleteItem(item) {
      this.deletingID = item.jellyfin_item_id;
      this.dueError = '';
      try {
        const res = await fetch('/api/due/delete', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ jellyfin_item_id: item.jellyfin_item_id }),
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

    async loadSettings() {
      const res = await fetch('/api/settings');
      if (!res.ok) return;
      this.settings = await res.json();
    },

    async saveSettings() {
      this.settingsError = '';
      this.settingsSaved = false;
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
        await this.loadStatus();
        this.settingsSaved = true;
        setTimeout(() => { this.settingsSaved = false; }, 3000);
      } catch (err) {
        this.settingsError = err.message;
      }
    },

    async loadConnections() {
      const res = await fetch('/api/connections');
      if (!res.ok) return;
      this.connections = await res.json();
    },

    async saveConnections() {
      this.connectionsError = '';
      this.connectionsSaved = false;
      try {
        const payload = {};
        for (const svc of ['jellyfin', 'radarr', 'sonarr', 'seerr']) {
          payload[svc] = {
            url: this.connections.values[svc].url,
            api_key: this.connectionKeyInputs[svc] || '',
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
        this.connections = await res.json();
        this.connectionKeyInputs = { jellyfin: '', radarr: '', sonarr: '', seerr: '' };
        await this.loadStatus();
        this.connectionsSaved = true;
        setTimeout(() => { this.connectionsSaved = false; }, 3000);
      } catch (err) {
        this.connectionsError = err.message;
      }
    },

    async testConnection(service) {
      this.testingService = service;
      this.testResults = { ...this.testResults, [service]: undefined };
      try {
        const res = await fetch('/api/connections/test', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            service,
            url: this.connections.values[service].url,
            api_key: this.connectionKeyInputs[service] || '',
          }),
        });
        const data = await res.json();
        this.testResults = { ...this.testResults, [service]: data.ok ? true : (data.error || 'Connection failed.') };
      } catch (err) {
        this.testResults = { ...this.testResults, [service]: err.message };
      } finally {
        this.testingService = null;
      }
    },

    formatTime(iso) {
      if (!iso) return '—';
      try {
        return new Date(iso).toLocaleString();
      } catch {
        return iso;
      }
    },
  };
}

PetiteVue.createApp({ App }).mount('#app');
