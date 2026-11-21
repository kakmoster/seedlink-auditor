(() => {
  "use strict";

  const state = {
    config: {
      read_only_mode: true,
      folders: [],
      qbit_instances: [],
      arr_instances: [],
      ignore_extensions: [],
      min_size_bytes: 0,
    },
    draftConfig: null,
    scan: null,
    activeTab: "ALL",
    search: "",
    seedingFilter: "ALL",
    arrFilter: "ALL",
    folderFilter: "ALL",
    sortBy: "default",
    viewMode: "grouped",
    page: 1,
    pageSize: 100,
    selectedGroupIds: new Set(),
    expandedPackIds: new Set(),
    expandedGroupIds: new Set(),
    currentPackMap: new Map(),
    pendingArrRefreshGroups: new Set(),
    activeSettingsTab: "folders",
    scanEpoch: 0,
    actionPreviewEpoch: 0,
    lastInspectResult: null,
    pickerTargetIndex: null,
    pickerCurrentPath: "",
    pickerParentPath: null,
    pendingAction: null,
  };

  const $ = (id) => document.getElementById(id);

  function escapeHtml(value) {
    return String(value ?? "")
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
  }

  function formatBytes(bytes) {
    const n = Number(bytes || 0);
    if (n <= 0) return "0 B";
    const units = ["B", "KiB", "MiB", "GiB", "TiB"];
    const exp = Math.min(Math.floor(Math.log(n) / Math.log(1024)), units.length - 1);
    const val = n / Math.pow(1024, exp);
    return `${val >= 100 || exp === 0 ? val.toFixed(0) : val.toFixed(2)} ${units[exp]}`;
  }

  function randomId(prefix) {
    return `${prefix}-${Math.random().toString(36).slice(2, 8)}`;
  }

  function showToast(message, type = "info") {
    const container = $("toastContainer");
    const toast = document.createElement("div");
    toast.className = `toast toast-${type}`;
    toast.textContent = message;
    container.appendChild(toast);
    setTimeout(() => {
      toast.remove();
    }, 4500);
  }

  async function apiFetch(url, options = {}) {
    const resp = await fetch(url, {
      headers: { "Content-Type": "application/json", ...(options.headers || {}) },
      ...options,
    });
    const text = await resp.text();
    let data = null;
    try {
      data = text ? JSON.parse(text) : null;
    } catch {
      data = { detail: text };
    }
    if (!resp.ok) {
      const msg = (data && data.detail) || `HTTP ${resp.status}`;
      throw new Error(msg);
    }
    return data;
  }

  function renderHeaderReadOnly() {
    const btn = $("readOnlyToggleBtn");
    const label = $("readOnlyLabel");
    if (state.config.read_only_mode) {
      btn.classList.add("is-safe");
      btn.classList.remove("is-unlocked");
      label.textContent = "SAFE READ-ONLY MODE";
    } else {
      btn.classList.remove("is-safe");
      btn.classList.add("is-unlocked");
      label.textContent = "ACTIONS ENABLED (WRITE MODE)";
    }
  }

  function renderTargetsStrip() {
    const folderChips = $("folderChips");
    const qbitChips = $("qbitChips");
    const arrChips = $("arrChips");
    const folderFilter = $("folderFilter");

    const folders = state.config.folders || [];
    if (folders.length === 0) {
      folderChips.innerHTML = `<span class="chip chip-muted">No folders configured</span>`;
    } else {
      folderChips.innerHTML = folders
        .map((f, idx) => {
          const roleTag = f.role === "primary" ? "A • Primary" : `B${idx > 1 ? idx : ""} • Secondary`;
          const isPrimary = f.role === "primary";
          return `<span class="chip ${isPrimary ? "chip-primary" : ""}" title="${escapeHtml(f.path)}">
            <strong>${escapeHtml(roleTag)}:</strong> ${escapeHtml(f.label)}
          </span>`;
        })
        .join("");
    }

    const prevFolderFilter = state.folderFilter;
    folderFilter.innerHTML = `<option value="ALL">All Folders</option>` +
      folders.map((f) => `<option value="${escapeHtml(f.id)}">${escapeHtml(f.label)}</option>`).join("");
    if ([...folderFilter.options].some((o) => o.value === prevFolderFilter)) {
      folderFilter.value = prevFolderFilter;
    } else {
      state.folderFilter = "ALL";
    }

    const instances = state.config.qbit_instances || [];
    const scanInstances = (state.scan && state.scan.qbit_instances) || [];
    if (instances.length === 0) {
      qbitChips.innerHTML = `<span class="chip chip-muted">No instances configured</span>`;
    } else {
      qbitChips.innerHTML = instances
        .map((inst) => {
          const rep = scanInstances.find((r) => r.instance_id === inst.id);
          let cls = "";
          let suffix = inst.enabled ? "" : " (disabled)";
          if (rep) {
            if (rep.ok && inst.enabled) {
              cls = "chip-ok";
              suffix = ` (${rep.torrent_count} torrents)`;
            } else if (!rep.ok) {
              cls = "chip-err";
              suffix = " (error)";
            }
          }
          return `<span class="chip ${cls}" title="${escapeHtml(inst.url)}">${escapeHtml(inst.name)}${escapeHtml(suffix)}</span>`;
        })
        .join("");
    }

    const arrInstances = state.config.arr_instances || [];
    const scanArrInstances = (state.scan && state.scan.arr_instances) || [];
    if (arrInstances.length === 0) {
      arrChips.innerHTML = `<span class="chip chip-muted">No instances configured</span>`;
    } else {
      arrChips.innerHTML = arrInstances
        .map((inst) => {
          const rep = scanArrInstances.find((r) => r.instance_id === inst.id);
          let cls = "";
          let suffix = inst.enabled ? "" : " (disabled)";
          if (rep) {
            if (rep.ok && inst.enabled) {
              cls = "chip-ok";
              suffix = ` (${rep.item_count} items)`;
            } else if (!rep.ok) {
              cls = "chip-err";
              suffix = " (error)";
            }
          }
          return `<span class="chip ${cls}" title="${escapeHtml(inst.url)}">${escapeHtml(inst.name)}${escapeHtml(suffix)}</span>`;
        })
        .join("");
    }

    if (state.scan) {
      const s = state.scan.summary;
      $("scanMeta").textContent =
        `Scanned ${s.total_files_scanned} files in ${state.scan.duration_ms} ms • Logical: ${formatBytes(s.logical_bytes)} • Physical: ${formatBytes(s.physical_bytes)}`;
    } else {
      $("scanMeta").textContent = "Not scanned yet";
    }
  }

  function renderAlerts() {
    const stack = $("alertStack");
    const warnings = (state.scan && state.scan.warnings) || [];
    if (warnings.length === 0) {
      stack.hidden = true;
      stack.innerHTML = "";
      return;
    }
    stack.hidden = false;
    stack.innerHTML = warnings
      .map((w) => `<div class="alert alert-warning">${escapeHtml(w)}</div>`)
      .join("");
  }

  function renderSummary() {
    const hasScan = Boolean(state.scan);
    $("welcomeCard").hidden = hasScan && state.config.folders.length > 0;

    if (!hasScan) {
      $("kpiOrphanedBytes").textContent = "0 B";
      $("kpiOrphanedCount").textContent = "0";
      $("kpiDuplicateBytes").textContent = "0 B";
      $("kpiDuplicateCount").textContent = "0";
      $("kpiHardlinkBytes").textContent = "0 B";
      $("kpiHardlinkCount").textContent = "0";
      $("kpiUntrackedBytes").textContent = "0 B";
      $("kpiUntrackedCount").textContent = "0";
      return;
    }

    const s = state.scan.summary;
    $("kpiOrphanedBytes").textContent = formatBytes(s.orphaned_in_b_reclaimable_bytes);
    $("kpiOrphanedCount").textContent = String(s.orphaned_in_b_count);
    $("kpiDuplicateBytes").textContent = formatBytes(s.duplicate_copy_reclaimable_bytes);
    $("kpiDuplicateCount").textContent = String(s.duplicate_copy_count);
    $("kpiHardlinkBytes").textContent = formatBytes(s.hardlinked_saved_bytes);
    $("kpiHardlinkCount").textContent = String(s.hardlinked_count);
    $("kpiUntrackedBytes").textContent = formatBytes(s.untracked_bytes);
    $("kpiUntrackedCount").textContent = String(s.untracked_count);

    $("tabCountAll").textContent = String(s.total_groups);
    $("tabCountOrphaned").textContent = String(s.orphaned_in_b_count);
    $("tabCountDup").textContent = String(s.duplicate_copy_count);
    $("tabCountHardlink").textContent = String(s.hardlinked_count);
    $("tabCountUntracked").textContent = String(s.untracked_count);
    $("tabCountTrackerIssue").textContent = String(s.tracker_error_count || 0);
    $("tabCountArrIssue").textContent = String(s.arr_issue_count || 0);
    $("tabCountOnlyA").textContent = String(s.only_in_a_count);
  }

  const STATUS_PRIORITY = {
    ORPHANED_IN_B: 0,
    DUPLICATE_COPY: 1,
    SAME_NAME_DIFF_SIZE: 2,
    HARDLINKED: 3,
    ONLY_IN_A: 4,
  };

  function getStatusPriority(status) {
    return Object.prototype.hasOwnProperty.call(STATUS_PRIORITY, status) ? STATUS_PRIORITY[status] : 9;
  }

  function sortDisplayRows(list, sortBy) {
    const copy = list.slice();
    copy.sort((a, b) => {
      if (sortBy === "reclaim_desc") {
        if ((b.reclaimable_bytes || 0) !== (a.reclaimable_bytes || 0)) {
          return (b.reclaimable_bytes || 0) - (a.reclaimable_bytes || 0);
        }
      } else if (sortBy === "size_desc") {
        if ((b.size || 0) !== (a.size || 0)) {
          return (b.size || 0) - (a.size || 0);
        }
      } else if (sortBy === "name_desc") {
        return String(b.title || b.filename || "").localeCompare(String(a.title || a.filename || ""), undefined, {
          numeric: true,
          sensitivity: "base",
        });
      } else if (sortBy === "name_asc") {
        return String(a.title || a.filename || "").localeCompare(String(b.title || b.filename || ""), undefined, {
          numeric: true,
          sensitivity: "base",
        });
      } else {
        if ((a.statusPriority ?? 9) !== (b.statusPriority ?? 9)) {
          return (a.statusPriority ?? 9) - (b.statusPriority ?? 9);
        }
        if ((b.reclaimable_bytes || 0) !== (a.reclaimable_bytes || 0)) {
          return (b.reclaimable_bytes || 0) - (a.reclaimable_bytes || 0);
        }
      }
      return String(a.title || a.filename || "").localeCompare(String(b.title || b.filename || ""), undefined, {
        numeric: true,
        sensitivity: "base",
      });
    });
    return copy;
  }

  function buildPackDisplayRows(filteredGroups) {
    const asFileItem = (grp) => ({
      kind: "file",
      id: grp.id,
      group: grp,
      members: [grp],
      title: grp.filename,
      filename: grp.filename,
      size: grp.size || 0,
      reclaimable_bytes: grp.reclaimable_bytes || 0,
      statusPriority: getStatusPriority(grp.status),
    });

    if (state.viewMode !== "grouped") {
      return sortDisplayRows(filteredGroups.map(asFileItem), state.sortBy);
    }

    const n = filteredGroups.length;
    if (n === 0) return [];

    const parent = Array.from({ length: n }, (_, i) => i);
    const find = (x) => {
      while (parent[x] !== x) {
        parent[x] = parent[parent[x]];
        x = parent[x];
      }
      return x;
    };
    const unite = (a, b) => {
      const ra = find(a);
      const rb = find(b);
      if (ra !== rb) parent[rb] = ra;
    };

    const byTorrentKey = new Map();
    for (let i = 0; i < n; i++) {
      const grp = filteredGroups[i];
      for (const t of grp.torrents || []) {
        const hashLower = String(t.torrent_hash || "").toLowerCase();
        if (!t.instance_id || !hashLower) continue;
        const tk = `${t.instance_id}::${hashLower}`;
        let entry = byTorrentKey.get(tk);
        if (!entry) {
          entry = { indices: [], torrent: t };
          byTorrentKey.set(tk, entry);
        }
        entry.indices.push(i);
      }
    }

    for (const entry of byTorrentKey.values()) {
      if (entry.indices.length < 2) continue;
      const first = entry.indices[0];
      for (let k = 1; k < entry.indices.length; k++) {
        unite(first, entry.indices[k]);
      }
    }

    const components = new Map();
    for (let i = 0; i < n; i++) {
      const root = find(i);
      let list = components.get(root);
      if (!list) {
        list = [];
        components.set(root, list);
      }
      list.push(i);
    }

    const displayRows = [];
    for (const idxs of components.values()) {
      if (idxs.length === 1) {
        displayRows.push(asFileItem(filteredGroups[idxs[0]]));
        continue;
      }

      const members = idxs
        .map((i) => filteredGroups[i])
        .sort((a, b) =>
          String(a.filename || "").localeCompare(String(b.filename || ""), undefined, {
            numeric: true,
            sensitivity: "base",
          })
        );

      const torrentStats = new Map();
      for (const m of members) {
        const seenInMember = new Set();
        for (const t of m.torrents || []) {
          const hk = `${t.instance_id}::${String(t.torrent_hash || "").toLowerCase()}`;
          if (!torrentStats.has(hk)) {
            torrentStats.set(hk, { torrent: t, memberCount: 0 });
          }
          if (!seenInMember.has(hk)) {
            seenInMember.add(hk);
            torrentStats.get(hk).memberCount++;
          }
        }
      }

      const rankedTorrents = [...torrentStats.values()].sort((a, b) => {
        if (b.memberCount !== a.memberCount) return b.memberCount - a.memberCount;
        const bfc = b.torrent.torrent_file_count || 0;
        const afc = a.torrent.torrent_file_count || 0;
        if (bfc !== afc) return bfc - afc;
        const aKey = `${a.torrent.instance_id}::${String(a.torrent.torrent_hash || "").toLowerCase()}`;
        const bKey = `${b.torrent.instance_id}::${String(b.torrent.torrent_hash || "").toLowerCase()}`;
        return aKey.localeCompare(bKey);
      });

      const primaryTorrent = rankedTorrents[0] ? rankedTorrents[0].torrent : null;
      const uniqueTorrents = rankedTorrents.map((r) => r.torrent);
      const packId = primaryTorrent
        ? `pack:${primaryTorrent.instance_id}:${String(primaryTorrent.torrent_hash || "").toLowerCase()}`
        : `pack:${members.map((m) => m.id).join(",")}`;
      const title = (primaryTorrent && primaryTorrent.torrent_name) || members[0].filename || "Torrent Pack";
      const totalTorrentFiles = uniqueTorrents.reduce((mx, t) => Math.max(mx, t.torrent_file_count || 0), 0);

      let size = 0;
      let reclaimable = 0;
      const statusCounts = {};
      const arrCounts = {};
      let anySeeding = false;
      let anyUnregistered = false;
      let anyError = false;
      let anyDownloading = false;
      let hasArrIssue = false;
      let hasCanImport = false;
      let hasCanRescan = false;
      let canHardlink = false;
      let canDeleteFiles = false;
      let anySyncingArr = false;
      let maxNlink = 0;
      const foldersA = new Map();
      const foldersB = new Map();
      let repArrMatch = null;

      for (const m of members) {
        size += m.size || 0;
        reclaimable += m.reclaimable_bytes || 0;
        statusCounts[m.status] = (statusCounts[m.status] || 0) + 1;
        const as = m.arr_summary || "NOT_IN_ARR";
        arrCounts[as] = (arrCounts[as] || 0) + 1;
        if (m.has_arr_issue) hasArrIssue = true;
        if ((m.arr_matches || []).some((a) => a.can_import)) hasCanImport = true;
        if ((m.arr_matches || []).some((a) => a.can_rescan)) hasCanRescan = true;
        if (m.can_hardlink && (m.hardlink_pairs || []).length > 0) canHardlink = true;
        if ((m.all_paths || []).length > 0 && (m.seeding_summary === "UNTRACKED" || m.status === "ORPHANED_IN_B")) {
          canDeleteFiles = true;
        }
        if (state.pendingArrRefreshGroups.has(m.id)) anySyncingArr = true;
        for (const f of m.folder_a_files || []) {
          if (!foldersA.has(f.folder_label)) foldersA.set(f.folder_label, f.abs_path);
          if ((f.nlink || 0) > maxNlink) maxNlink = f.nlink || 0;
        }
        for (const f of m.folder_b_files || []) {
          if (!foldersB.has(f.folder_label)) foldersB.set(f.folder_label, f.abs_path);
          if ((f.nlink || 0) > maxNlink) maxNlink = f.nlink || 0;
        }
        if (!repArrMatch && (m.arr_matches || []).length > 0) {
          repArrMatch = m.arr_matches[0];
        }
      }

      for (const t of uniqueTorrents) {
        if (t.tracker_health === "UNREGISTERED") anyUnregistered = true;
        else if (t.tracker_health === "TRACKER_ERROR" || t.tracker_health === "LOCAL_ERROR") anyError = true;
        if (t.is_seeding) anySeeding = true;
        else if (t.seeding_status === "downloading") anyDownloading = true;
      }

      let seedingSummary = "UNTRACKED";
      if (anyUnregistered && !anySeeding) seedingSummary = "UNREGISTERED";
      else if (anyError && !anySeeding) seedingSummary = "ERROR";
      else if (anySeeding) seedingSummary = "SEEDING";
      else if (anyDownloading) seedingSummary = "DOWNLOADING";
      else if (uniqueTorrents.length > 0) seedingSummary = "PAUSED";

      const orderedStatuses = Object.keys(statusCounts).sort((a, b) => getStatusPriority(a) - getStatusPriority(b));
      const primaryStatus = orderedStatuses[0] || members[0].status;
      const arrPriority = [
        "IN_QUEUE_BLOCKED",
        "WANTED_NOT_IMPORTED",
        "UNLINKED_IN_LIBRARY",
        "REPLACED_IN_ARR",
        "IMPORTED",
        "NOT_IN_ARR",
        "UNCONFIGURED",
      ];
      let primaryArrSummary = members[0].arr_summary || "NOT_IN_ARR";
      for (const ap of arrPriority) {
        if (arrCounts[ap] > 0) {
          primaryArrSummary = ap;
          break;
        }
      }

      displayRows.push({
        kind: "pack",
        id: packId,
        title,
        filename: title,
        members,
        primaryTorrent,
        uniqueTorrents,
        torrents: uniqueTorrents,
        totalTorrentFiles,
        size,
        reclaimable_bytes: reclaimable,
        status: primaryStatus,
        statusPriority: getStatusPriority(primaryStatus),
        statusCounts,
        hasMixedStatus: orderedStatuses.length > 1,
        sole_copy_on_disk: members.every((m) => m.sole_copy_on_disk),
        external_hardlink_count: members.reduce((mx, m) => Math.max(mx, m.external_hardlink_count || 0), 0),
        seeding_summary: seedingSummary,
        has_tracker_issue: anyUnregistered || anyError,
        has_unregistered: anyUnregistered,
        arr_summary: primaryArrSummary,
        arrCounts,
        hasMixedArr: Object.keys(arrCounts).length > 1,
        hasArrIssue,
        repArrMatch,
        hasCanImport,
        hasCanRescan,
        hasErroredTorrents: uniqueTorrents.some((t) => Boolean(t.tracker_health)),
        canHardlink,
        canDeleteFiles,
        anySyncingArr,
        foldersA,
        foldersB,
        maxNlink,
      });
    }

    return sortDisplayRows(displayRows, state.sortBy);
  }

  function getFilteredGroups() {
    if (!state.scan || !Array.isArray(state.scan.groups)) return [];
    const q = state.search.trim().toLowerCase();

    return state.scan.groups.filter((grp) => {
      if (state.activeTab === "ARR_ISSUE") {
        if (!grp.has_arr_issue) return false;
      } else if (state.activeTab === "TRACKER_ISSUE") {
        if (!grp.has_tracker_issue) return false;
      } else if (state.activeTab === "UNTRACKED") {
        if (grp.seeding_summary !== "UNTRACKED") return false;
      } else if (state.activeTab !== "ALL") {
        if (grp.status !== state.activeTab) return false;
      }

      if (state.seedingFilter === "UNREGISTERED") {
        if (!grp.has_unregistered && grp.seeding_summary !== "UNREGISTERED") return false;
      } else if (state.seedingFilter === "ERROR") {
        if (!grp.has_tracker_issue && grp.seeding_summary !== "ERROR") return false;
      } else if (state.seedingFilter !== "ALL" && grp.seeding_summary !== state.seedingFilter) {
        return false;
      }

      if (state.arrFilter !== "ALL" && grp.arr_summary !== state.arrFilter) {
        return false;
      }

      if (state.folderFilter !== "ALL") {
        const files = [...grp.folder_a_files, ...grp.folder_b_files];
        if (!files.some((f) => f.folder_id === state.folderFilter)) return false;
      }

      if (q) {
        const hay = [
          grp.filename,
          grp.rel_path,
          grp.arr_summary || "",
          ...grp.all_paths,
          ...grp.torrents.map(
            (t) => `${t.torrent_name} ${t.tracker_host} ${t.instance_name} ${t.tracker_health || ""} ${t.tracker_msg || ""}`
          ),
          ...(grp.arr_matches || []).map(
            (m) => `${m.instance_name} ${m.app_type} ${m.title} ${m.status_message || ""} ${m.existing_file_name || ""}`
          ),
        ]
          .join(" ")
          .toLowerCase();
        if (!hay.includes(q)) return false;
      }

      return true;
    });
  }

  function renderStatusBadge(grp) {
    switch (grp.status) {
      case "HARDLINKED":
        return `<span class="status-pill pill-hardlink">HARDLINKED</span>`;
      case "DUPLICATE_COPY":
        return `<span class="status-pill pill-duplicate">DUPLICATE COPY</span>`;
      case "ORPHANED_IN_B":
        return grp.sole_copy_on_disk
          ? `<span class="status-pill pill-orphaned">ORPHANED IN B (SOLE COPY)</span>`
          : `<span class="status-pill pill-orphaned">ORPHANED IN B (${grp.external_hardlink_count} EXT LINKS)</span>`;
      case "ONLY_IN_A":
        return `<span class="status-pill pill-only-a">ONLY IN FOLDER A</span>`;
      case "SAME_NAME_DIFF_SIZE":
        return `<span class="status-pill pill-diff-size">SAME NAME / DIFF SIZE</span>`;
      default:
        return `<span class="status-pill pill-diff-size">${escapeHtml(grp.status)}</span>`;
    }
  }

  function trackerHealthLabel(health) {
    switch (health) {
      case "UNREGISTERED":
        return "UNREGISTERED";
      case "LOCAL_ERROR":
        return "LOCAL ERROR";
      case "TRACKER_ERROR":
        return "TRACKER ERROR";
      default:
        return health || "";
    }
  }

  function renderSeedingBadge(grp) {
    let primary = "";
    switch (grp.seeding_summary) {
      case "SEEDING":
        primary = `<span class="status-pill pill-seeding">SEEDING</span>`;
        break;
      case "UNREGISTERED":
        primary = `<span class="status-pill pill-error">UNREGISTERED</span>`;
        break;
      case "ERROR":
        primary = `<span class="status-pill pill-error">TRACKER ERROR</span>`;
        break;
      case "PAUSED":
        primary = `<span class="status-pill pill-paused">PAUSED</span>`;
        break;
      case "DOWNLOADING":
        primary = `<span class="status-pill pill-downloading">DOWNLOADING</span>`;
        break;
      default:
        primary = `<span class="status-pill pill-untracked">UNTRACKED</span>`;
        break;
    }

    if (grp.has_tracker_issue && grp.seeding_summary !== "UNREGISTERED" && grp.seeding_summary !== "ERROR") {
      const issueLabel = grp.has_unregistered ? "HAS UNREGISTERED" : "HAS TRACKER ERROR";
      return `${primary} <span class="status-pill pill-error">${issueLabel}</span>`;
    }
    return primary;
  }

  function renderFolderFileBox(file, badgeLabel) {
    return `
      <div class="folder-entry">
        <div class="folder-entry-head">
          <span class="folder-role-tag">${escapeHtml(badgeLabel)}: ${escapeHtml(file.folder_label)}</span>
          <span>
            <span class="inode-tag">#${escapeHtml(file.inode_str || file.inode)}</span>
            <span class="nlink-badge">nlink:${escapeHtml(file.nlink)}</span>
          </span>
        </div>
        <div class="folder-rel-path" title="${escapeHtml(file.abs_path)}">${escapeHtml(file.rel_path)}</div>
      </div>
    `;
  }

  function renderArrBadge(arrSummary) {
    switch (arrSummary) {
      case "IMPORTED":
        return `<span class="status-pill pill-arr-ok">IMPORTED</span>`;
      case "WANTED_NOT_IMPORTED":
        return `<span class="status-pill pill-arr-warn">WANTED (NOT IMPORTED)</span>`;
      case "IN_QUEUE_BLOCKED":
        return `<span class="status-pill pill-arr-danger">QUEUE BLOCKED</span>`;
      case "UNLINKED_IN_LIBRARY":
        return `<span class="status-pill pill-arr-warn">UNLINKED IN LIBRARY</span>`;
      case "REPLACED_IN_ARR":
        return `<span class="status-pill pill-arr-muted">REPLACED IN ARR</span>`;
      case "UNCONFIGURED":
        return `<span class="status-pill pill-arr-muted">NOT CONFIGURED</span>`;
      default:
        return `<span class="status-pill pill-arr-muted">NOT IN ARR</span>`;
    }
  }

  function renderArrTitle(m) {
    if (m.app_type === "sonarr" && (m.season_number > 0 || m.episode_number > 0)) {
      const epStr = `S${String(m.season_number || 0).padStart(2, "0")}E${String(m.episode_number || 0).padStart(2, "0")}`;
      return `${m.title} ${epStr}`;
    }
    return m.year > 0 ? `${m.title} (${m.year})` : m.title;
  }

  function renderGroupDetailRow(grp, isPackChild = false) {
    const missingAHtml =
      grp.status === "ORPHANED_IN_B"
        ? `<div class="missing-in-a">Missing from Folder A (original deleted)</div>`
        : "";

    const folderHtml =
      missingAHtml +
      grp.folder_a_files.map((f) => renderFolderFileBox(f, "Folder A")).join("") +
      grp.folder_b_files.map((f) => renderFolderFileBox(f, "Folder B")).join("");

    const torrentsHtml =
      grp.torrents.length === 0
        ? `<div class="torrent-sub">Not matched in any configured qBittorrent instance</div>`
        : grp.torrents
            .map((t) => {
              const hasHealthIssue = Boolean(t.tracker_health);
              const healthBadge = hasHealthIssue
                ? `<span class="status-pill pill-error pill-inline">${escapeHtml(trackerHealthLabel(t.tracker_health))}</span>`
                : "";
              const fileCountBadge = t.torrent_file_count > 0
                ? `<span class="nlink-badge">${t.torrent_file_count === 1 ? "1 file" : `${escapeHtml(t.torrent_file_count)} files`}</span>`
                : "";
              const msgHtml = t.tracker_msg
                ? `<div class="tracker-error-msg">${escapeHtml(t.tracker_msg)}</div>`
                : "";
              return `
            <div class="torrent-item ${hasHealthIssue ? "has-error" : ""}">
              <div class="torrent-top">
                <span class="instance-badge">${escapeHtml(t.instance_name)}</span>
                ${t.tracker_host ? `<span class="tracker-badge">${escapeHtml(t.tracker_host)}</span>` : ""}
                <span class="nlink-badge">${escapeHtml(t.state)}</span>
                ${fileCountBadge}
                ${healthBadge}
              </div>
              <div class="torrent-name">${escapeHtml(t.torrent_name)}</div>
              ${msgHtml}
              <div class="torrent-sub">Ratio: ${Number(t.ratio || 0).toFixed(2)} • Match: ${escapeHtml(t.match_type)} • Save: ${escapeHtml(t.save_path || "")}</div>
            </div>`;
            })
            .join("");

    const arrMatches = grp.arr_matches || [];
    const arrHtml =
      grp.arr_summary === "UNCONFIGURED"
        ? `<div class="torrent-sub">Configure Radarr or Sonarr in Settings</div>`
        : arrMatches.length === 0
          ? `<div class="torrent-sub">No matching reference found</div>`
          : arrMatches
              .map((m) => {
                const isIssue = m.can_import || m.can_rescan;
                const monLabel = m.monitored ? "Monitored" : "Unmonitored";
                return `
              <div class="arr-item ${isIssue ? "has-issue" : ""}">
                <div class="torrent-top">
                  <span class="arr-type-badge">${escapeHtml(m.app_type)}</span>
                  <span class="instance-badge">${escapeHtml(m.instance_name)}</span>
                  <span class="nlink-badge">${monLabel}</span>
                </div>
                <div class="torrent-name">${escapeHtml(renderArrTitle(m))}</div>
                ${
                  m.existing_file_name
                    ? `<div class="torrent-sub">Arr already has another file:</div><div class="arr-existing-file" title="${escapeHtml(m.existing_file_path || m.existing_file_name)}">${escapeHtml(m.existing_file_name)}</div>`
                    : m.status_message
                      ? `<div class="${isIssue ? "arr-msg" : "torrent-sub"}">${escapeHtml(m.status_message)}</div>`
                      : ""
                }
                ${m.root_path ? `<div class="torrent-sub" title="${escapeHtml(m.root_path)}">Library: ${escapeHtml(m.root_path)}</div>` : ""}
              </div>`;
              })
              .join("");

    return `
      <tr class="detail-row ${isPackChild ? "pack-child-detail-row" : ""}" data-detail-for="${escapeHtml(grp.id)}">
        <td colspan="7">
          <div class="detail-panel">
            <div class="detail-grid">
              <div>
                <div class="detail-col-title">Disk Locations &amp; Inodes</div>
                ${folderHtml}
              </div>
              <div>
                <div class="detail-col-title">qBittorrent (${grp.torrents.length})</div>
                ${torrentsHtml}
              </div>
              <div>
                <div class="detail-col-title">Radarr / Sonarr (${arrMatches.length})</div>
                ${arrHtml}
              </div>
            </div>
          </div>
        </td>
      </tr>
    `;
  }

  function formatCompactFolders(grp) {
    const seen = new Set();
    const chips = [];
    for (const f of grp.folder_a_files || []) {
      const k = `A:${f.folder_label}`;
      if (seen.has(k)) continue;
      seen.add(k);
      chips.push(`<span class="mini-chip mini-chip-primary" title="Folder A: ${escapeHtml(f.abs_path)}">${escapeHtml(f.folder_label)}</span>`);
    }
    for (const f of grp.folder_b_files || []) {
      const k = `B:${f.folder_label}`;
      if (seen.has(k)) continue;
      seen.add(k);
      chips.push(`<span class="mini-chip" title="Folder B: ${escapeHtml(f.abs_path)}">${escapeHtml(f.folder_label)}</span>`);
    }
    const maxNlink = Math.max(0, ...[...(grp.folder_a_files || []), ...(grp.folder_b_files || [])].map((f) => f.nlink || 0));
    const nlinkTag = maxNlink > 0 ? `<span class="nlink-badge">nlink:${maxNlink}</span>` : "";
    return `<div class="cell-badges">${chips.join("")}${nlinkTag}</div>`;
  }

  function formatCompactQbitSummary(grp) {
    const list = grp.torrents || [];
    if (list.length === 0) {
      return `<div class="cell-sub-trunc">Not in qBit</div>`;
    }
    if (list.length === 1) {
      const t = list[0];
      const msg = t.tracker_msg ? ` • ${t.tracker_msg}` : "";
      const summary = `${t.instance_name} • ${t.tracker_host || t.state} • R:${Number(t.ratio || 0).toFixed(2)}${msg}`;
      return `<div class="cell-sub-trunc" title="${escapeHtml(t.torrent_name)}">${escapeHtml(summary)}</div>`;
    }
    const unregCount = list.filter((t) => t.tracker_health === "UNREGISTERED").length;
    const errCount = list.filter((t) => Boolean(t.tracker_health) && t.tracker_health !== "UNREGISTERED").length;
    const issueParts = [];
    if (unregCount > 0) issueParts.push(`${unregCount} unreg`);
    if (errCount > 0) issueParts.push(`${errCount} err`);
    const hosts = [...new Set(list.map((t) => t.tracker_host).filter(Boolean))].slice(0, 2).join(", ");
    const extra = issueParts.length > 0 ? ` (${issueParts.join(", ")})` : hosts ? ` • ${hosts}` : "";
    return `<div class="cell-sub-trunc">${list.length} torrents${escapeHtml(extra)}</div>`;
  }

  function formatCompactArrSummary(grp) {
    const list = grp.arr_matches || [];
    if (grp.arr_summary === "UNCONFIGURED") {
      return `<div class="cell-sub-trunc">Not configured</div>`;
    }
    if (list.length === 0) {
      return `<div class="cell-sub-trunc">No match</div>`;
    }
    const primary = list[0];
    if (primary.existing_file_name) {
      return `<div class="cell-sub-trunc" title="Arr already has: ${escapeHtml(primary.existing_file_path || primary.existing_file_name)}">Has: ${escapeHtml(primary.existing_file_name)}</div>`;
    }
    const label = `${primary.instance_name}: ${renderArrTitle(primary)}`;
    return `<div class="cell-sub-trunc" title="${escapeHtml(primary.status_message || label)}">${escapeHtml(label)}</div>`;
  }

  function shortStatusLabel(s) {
    switch (s) {
      case "ORPHANED_IN_B":
        return "Orphaned";
      case "DUPLICATE_COPY":
        return "Dup";
      case "HARDLINKED":
        return "Hardlinked";
      case "ONLY_IN_A":
        return "Only A";
      case "SAME_NAME_DIFF_SIZE":
        return "Diff Size";
      default:
        return s;
    }
  }

  function updatePaginationUi(totalDisplayCount, totalFileCount, startIdx, endIdx, totalPages, pageItems) {
    const bar = $("paginationBar");
    if (totalDisplayCount === 0) {
      bar.hidden = true;
    } else {
      bar.hidden = false;
      const suffix =
        state.viewMode === "grouped" && totalDisplayCount !== totalFileCount
          ? ` (${totalFileCount.toLocaleString()} files)`
          : "";
      $("paginationInfo").textContent = `Showing ${(startIdx + 1).toLocaleString()}\u2013${endIdx.toLocaleString()} of ${totalDisplayCount.toLocaleString()} item(s)${suffix}`;
      $("pageIndicator").textContent = `Page ${state.page} / ${totalPages}`;
      $("pageFirstBtn").disabled = state.page <= 1;
      $("pagePrevBtn").disabled = state.page <= 1;
      $("pageNextBtn").disabled = state.page >= totalPages;
      $("pageLastBtn").disabled = state.page >= totalPages;
    }

    const expandBtn = $("expandPageBtn");
    if (expandBtn) {
      const anyPageExpanded =
        pageItems.length > 0 &&
        pageItems.every((it) =>
          it.kind === "pack" ? state.expandedPackIds.has(it.id) : state.expandedGroupIds.has(it.id)
        );
      expandBtn.textContent = anyPageExpanded ? "Collapse Page" : "Expand Page";
      expandBtn.disabled = pageItems.length === 0;
    }
  }

  function renderFileRow(grp, isPackChild = false) {
    const checked = state.selectedGroupIds.has(grp.id) ? "checked" : "";
    const isExpanded = state.expandedGroupIds.has(grp.id);
    const arrMatches = grp.arr_matches || [];

    const reclaimClass =
      grp.reclaimable_bytes > 0
        ? grp.status === "ORPHANED_IN_B"
          ? "reclaim-danger"
          : "reclaim-highlight"
        : "reclaim-zero";

    const actionBtns = [];
    if (arrMatches.some((m) => m.can_import)) {
      actionBtns.push(
        `<button class="btn btn-amber btn-xs row-arr-import" data-group-id="${escapeHtml(grp.id)}" type="button" title="Tell Radarr/Sonarr to import this file into its library folder">Import</button>`
      );
    }
    if (arrMatches.some((m) => m.can_rescan)) {
      actionBtns.push(
        `<button class="btn btn-secondary btn-xs row-arr-rescan" data-group-id="${escapeHtml(grp.id)}" type="button" title="Scan Arr's existing library folder to link this file without moving it">Rescan</button>`
      );
    }
    const erroredTorrents = grp.torrents.filter((t) => Boolean(t.tracker_health));
    if (erroredTorrents.length > 0) {
      const errBtnText = grp.has_unregistered ? "Del Unreg" : "Del Err";
      const errTitle = grp.has_unregistered
        ? "Remove unregistered torrent(s) from qBittorrent (healthy torrents stay)"
        : "Remove errored torrent(s) from qBittorrent (healthy torrents stay)";
      actionBtns.push(
        `<button class="btn btn-outline-danger btn-xs row-delete-errored-torrent" data-group-id="${escapeHtml(grp.id)}" type="button" title="${errTitle}">${errBtnText}</button>`
      );
    }
    if (grp.torrents.length > 0) {
      actionBtns.push(
        `<button class="btn btn-danger btn-xs row-delete-torrent" data-group-id="${escapeHtml(grp.id)}" type="button" title="Remove matching torrent(s) from qBittorrent (with optional data file delete)">Del qBit</button>`
      );
    }
    if (grp.can_hardlink && grp.hardlink_pairs.length > 0) {
      actionBtns.push(
        `<button class="btn btn-amber btn-xs row-hardlink" data-group-id="${escapeHtml(grp.id)}" type="button" title="Replace duplicate copies with hardlinks to reclaim space">Hardlink</button>`
      );
    }
    if (grp.all_paths.length > 0 && (grp.seeding_summary === "UNTRACKED" || grp.status === "ORPHANED_IN_B")) {
      actionBtns.push(
        `<button class="btn btn-outline-danger btn-xs row-delete-file" data-group-id="${escapeHtml(grp.id)}" type="button" title="Delete file(s) directly from disk">Del File</button>`
      );
    }
    actionBtns.push(
      `<button class="btn btn-secondary btn-xs row-refresh" data-group-id="${escapeHtml(grp.id)}" type="button" title="Refresh disk, qBittorrent & Radarr/Sonarr status for this item">&#8635;</button>`
    );

    const arrSyncHtml = state.pendingArrRefreshGroups.has(grp.id)
      ? `<div class="arr-sync-badge">Syncing Arr...</div>`
      : "";

    const mainRowHtml = `
      <tr class="main-row ${isPackChild ? "pack-child-row" : ""} ${isExpanded ? "is-expanded" : ""}" data-group-id="${escapeHtml(grp.id)}">
        <td class="col-check">
          <div class="row-check-wrap">
            <input type="checkbox" class="row-checkbox" data-group-id="${escapeHtml(grp.id)}" ${checked} />
            <button class="row-expand-btn ${isExpanded ? "expanded" : ""}" data-group-id="${escapeHtml(grp.id)}" type="button" title="${isExpanded ? "Collapse details" : "Expand full paths, torrents & Arr details"}" aria-label="Toggle row details">&#9656;</button>
          </div>
        </td>
        <td class="col-file">
          <div class="file-name" title="${escapeHtml(grp.filename)}">${escapeHtml(grp.filename)}</div>
          <div class="file-meta">
            <span>${formatBytes(grp.size)}</span>
            <span>${escapeHtml(grp.ext || "file")}</span>
          </div>
        </td>
        <td class="col-folders">
          <div class="cell-stack">
            ${renderStatusBadge(grp)}
            ${formatCompactFolders(grp)}
          </div>
        </td>
        <td class="col-qbit">
          <div class="cell-stack">
            ${renderSeedingBadge(grp)}
            ${formatCompactQbitSummary(grp)}
          </div>
        </td>
        <td class="col-arr">
          <div class="cell-stack">
            ${renderArrBadge(grp.arr_summary)}
            ${formatCompactArrSummary(grp)}
            ${arrSyncHtml}
          </div>
        </td>
        <td class="col-reclaim">
          <div class="reclaim-bytes ${reclaimClass}">${grp.reclaimable_bytes > 0 ? formatBytes(grp.reclaimable_bytes) : "—"}</div>
          ${
            grp.status === "DUPLICATE_COPY" && !grp.can_hardlink
              ? `<div class="cell-sub-trunc" title="Cross-device (cannot hardlink)">Cross-dev</div>`
              : ""
          }
        </td>
        <td class="col-actions">
          <div class="row-actions">${actionBtns.join("")}</div>
        </td>
      </tr>
    `;

    return isExpanded ? mainRowHtml + renderGroupDetailRow(grp, isPackChild) : mainRowHtml;
  }

  function formatCompactPackFolders(pack) {
    const chips = [];
    for (const [label, path] of pack.foldersA.entries()) {
      chips.push(`<span class="mini-chip mini-chip-primary" title="Folder A: ${escapeHtml(path)}">${escapeHtml(label)}</span>`);
    }
    for (const [label, path] of pack.foldersB.entries()) {
      chips.push(`<span class="mini-chip" title="Folder B: ${escapeHtml(path)}">${escapeHtml(label)}</span>`);
    }
    const nlinkTag = pack.maxNlink > 0 ? `<span class="nlink-badge">nlink:${pack.maxNlink}</span>` : "";
    return `<div class="cell-badges">${chips.join("")}${nlinkTag}</div>`;
  }

  function formatCompactPackArr(pack) {
    if (pack.arr_summary === "UNCONFIGURED") {
      return `<div class="cell-sub-trunc">Not configured</div>`;
    }
    if (pack.hasMixedArr) {
      const labels = {
        IMPORTED: "imported",
        WANTED_NOT_IMPORTED: "wanted",
        IN_QUEUE_BLOCKED: "blocked",
        UNLINKED_IN_LIBRARY: "unlinked",
        REPLACED_IN_ARR: "replaced",
        NOT_IN_ARR: "not in arr",
      };
      const parts = Object.entries(pack.arrCounts).map(([k, v]) => `${v} ${labels[k] || k}`);
      return `<div class="cell-sub-trunc" title="${escapeHtml(parts.join(", "))}">${escapeHtml(parts.join(", "))}</div>`;
    }
    if (!pack.repArrMatch) {
      return `<div class="cell-sub-trunc">No match</div>`;
    }
    const m = pack.repArrMatch;
    if (m.existing_file_name) {
      return `<div class="cell-sub-trunc" title="${escapeHtml(m.existing_file_path || m.existing_file_name)}">Replaced in library</div>`;
    }
    return `<div class="cell-sub-trunc">${escapeHtml(m.instance_name)}: ${escapeHtml(m.title)} (${pack.members.length} files)</div>`;
  }

  function renderPackRow(pack) {
    const allSelected = pack.members.every((m) => state.selectedGroupIds.has(m.id));
    const isExpanded = state.expandedPackIds.has(pack.id);
    const reclaimClass =
      pack.reclaimable_bytes > 0
        ? pack.status === "ORPHANED_IN_B"
          ? "reclaim-danger"
          : "reclaim-highlight"
        : "reclaim-zero";

    const actionBtns = [];
    if (pack.hasCanImport) {
      actionBtns.push(
        `<button class="btn btn-amber btn-xs pack-arr-import" data-pack-id="${escapeHtml(pack.id)}" type="button" title="Import all eligible episodes in this pack into Radarr/Sonarr">Import Pack</button>`
      );
    }
    if (pack.hasCanRescan) {
      actionBtns.push(
        `<button class="btn btn-secondary btn-xs pack-arr-rescan" data-pack-id="${escapeHtml(pack.id)}" type="button" title="Rescan Arr's library folder for this pack">Rescan</button>`
      );
    }
    if (pack.hasErroredTorrents) {
      const errBtnText = pack.has_unregistered ? "Del Unreg" : "Del Err";
      actionBtns.push(
        `<button class="btn btn-outline-danger btn-xs pack-delete-errored-torrent" data-pack-id="${escapeHtml(pack.id)}" type="button" title="Remove errored/unregistered torrent(s) from qBittorrent">${errBtnText}</button>`
      );
    }
    if (pack.uniqueTorrents.length > 0) {
      actionBtns.push(
        `<button class="btn btn-danger btn-xs pack-delete-torrent" data-pack-id="${escapeHtml(pack.id)}" type="button" title="Remove matching torrent(s) for this pack from qBittorrent">Del qBit</button>`
      );
    }
    if (pack.canHardlink) {
      actionBtns.push(
        `<button class="btn btn-amber btn-xs pack-hardlink" data-pack-id="${escapeHtml(pack.id)}" type="button" title="Convert duplicate copies in this pack to hardlinks">Hardlink</button>`
      );
    }
    if (pack.canDeleteFiles) {
      actionBtns.push(
        `<button class="btn btn-outline-danger btn-xs pack-delete-file" data-pack-id="${escapeHtml(pack.id)}" type="button" title="Delete orphaned/untracked files in this pack directly from disk">Del Files</button>`
      );
    }
    actionBtns.push(
      `<button class="btn btn-secondary btn-xs pack-refresh" data-pack-id="${escapeHtml(pack.id)}" type="button" title="Refresh disk, qBittorrent & Arr status for all files in this pack">&#8635;</button>`
    );

    const fileMetaExtra =
      pack.totalTorrentFiles > pack.members.length
        ? `${pack.members.length} files (${pack.totalTorrentFiles} in torrent)`
        : `${pack.members.length} files`;

    const mixedStatusNote = pack.hasMixedStatus
      ? `<div class="cell-sub-trunc">${Object.entries(pack.statusCounts).map(([k, v]) => `${v} ${shortStatusLabel(k)}`).join(", ")}</div>`
      : formatCompactPackFolders(pack);

    const arrSyncHtml = pack.anySyncingArr ? `<div class="arr-sync-badge">Syncing Arr...</div>` : "";

    const packMainHtml = `
      <tr class="main-row pack-row ${isExpanded ? "is-expanded" : ""}" data-pack-id="${escapeHtml(pack.id)}">
        <td class="col-check">
          <div class="row-check-wrap">
            <input type="checkbox" class="pack-checkbox" data-pack-id="${escapeHtml(pack.id)}" ${allSelected ? "checked" : ""} />
            <button class="row-expand-btn pack-expand-btn ${isExpanded ? "expanded" : ""}" data-pack-id="${escapeHtml(pack.id)}" type="button" title="${isExpanded ? "Collapse pack files" : "Show all episodes/files inside this torrent pack"}" aria-label="Toggle pack files">&#9656;</button>
          </div>
        </td>
        <td class="col-file">
          <div class="file-name-wrap">
            <span class="pack-badge">PACK</span>
            <div class="file-name" title="${escapeHtml(pack.title)}">${escapeHtml(pack.title)}</div>
          </div>
          <div class="file-meta">
            <span>${formatBytes(pack.size)}</span>
            <span>${escapeHtml(fileMetaExtra)}</span>
          </div>
        </td>
        <td class="col-folders">
          <div class="cell-stack">
            ${renderStatusBadge(pack)}
            ${mixedStatusNote}
          </div>
        </td>
        <td class="col-qbit">
          <div class="cell-stack">
            ${renderSeedingBadge(pack)}
            ${formatCompactQbitSummary({ torrents: pack.uniqueTorrents })}
          </div>
        </td>
        <td class="col-arr">
          <div class="cell-stack">
            ${renderArrBadge(pack.arr_summary)}
            ${formatCompactPackArr(pack)}
            ${arrSyncHtml}
          </div>
        </td>
        <td class="col-reclaim">
          <div class="reclaim-bytes ${reclaimClass}">${pack.reclaimable_bytes > 0 ? formatBytes(pack.reclaimable_bytes) : "—"}</div>
        </td>
        <td class="col-actions">
          <div class="row-actions">${actionBtns.join("")}</div>
        </td>
      </tr>
    `;

    if (!isExpanded) return packMainHtml;
    return packMainHtml + pack.members.map((m) => renderFileRow(m, true)).join("");
  }

  function renderTable() {
    const tbody = $("auditTableBody");
    const filteredGroups = getFilteredGroups();
    const displayRows = buildPackDisplayRows(filteredGroups);
    state.currentPackMap.clear();
    for (const item of displayRows) {
      if (item.kind === "pack") {
        state.currentPackMap.set(item.id, item);
      }
    }

    if (!state.scan) {
      tbody.innerHTML = `<tr><td colspan="7" class="empty-table-cell">No scan data yet. Open <strong>Settings</strong> to add your folders and click <strong>Scan Now</strong>.</td></tr>`;
      updatePaginationUi(0, 0, 0, 0, 1, []);
      updateSelectionControls([], []);
      return;
    }

    if (displayRows.length === 0) {
      tbody.innerHTML = `<tr><td colspan="7" class="empty-table-cell">No files match the current filter criteria.</td></tr>`;
      updatePaginationUi(0, 0, 0, 0, 1, []);
      updateSelectionControls([], []);
      return;
    }

    const totalPages = Math.max(1, Math.ceil(displayRows.length / state.pageSize));
    if (state.page > totalPages) state.page = totalPages;
    if (state.page < 1) state.page = 1;
    const startIdx = (state.page - 1) * state.pageSize;
    const endIdx = Math.min(displayRows.length, startIdx + state.pageSize);
    const pageItems = displayRows.slice(startIdx, endIdx);

    tbody.innerHTML = pageItems
      .map((item) => (item.kind === "pack" ? renderPackRow(item) : renderFileRow(item.group, false)))
      .join("");

    // Apply indeterminate state on partially-selected pack checkboxes (DOM property only).
    tbody.querySelectorAll(".pack-checkbox").forEach((cb) => {
      const pack = state.currentPackMap.get(cb.dataset.packId);
      if (!pack) return;
      const selCount = pack.members.filter((m) => state.selectedGroupIds.has(m.id)).length;
      cb.checked = selCount === pack.members.length;
      cb.indeterminate = selCount > 0 && selCount < pack.members.length;
    });

    updatePaginationUi(displayRows.length, filteredGroups.length, startIdx, endIdx, totalPages, pageItems);
    updateSelectionControls(filteredGroups, pageItems);
  }

  function updateSelectionControls(filteredGroups, pageItems = []) {
    const pageGroupIds = new Set();
    for (const item of pageItems) {
      for (const m of item.members || [item.group]) {
        if (m && m.id) pageGroupIds.add(m.id);
      }
    }
    const selectedOnPage = [...state.selectedGroupIds].filter((id) => pageGroupIds.has(id));
    const selectAll = $("selectAllCheckbox");
    selectAll.checked = pageGroupIds.size > 0 && selectedOnPage.length === pageGroupIds.size;
    selectAll.indeterminate = selectedOnPage.length > 0 && selectedOnPage.length < pageGroupIds.size;

    const selectedGroups = getSelectedGroups();
    $("bulkBar").hidden = selectedGroups.length === 0;
    $("selectionBadge").textContent = `${selectedGroups.length.toLocaleString()} selected`;

    const allFilteredIds = new Set(filteredGroups.map((g) => g.id));
    const selectedInFilter = [...state.selectedGroupIds].filter((id) => allFilteredIds.has(id));
    const selAllMatchBtn = $("selectAllMatchingBtn");
    if (selAllMatchBtn) {
      if (selectedGroups.length > 0 && filteredGroups.length > pageGroupIds.size && selectedInFilter.length < filteredGroups.length) {
        selAllMatchBtn.hidden = false;
        selAllMatchBtn.textContent = `Select all ${filteredGroups.length.toLocaleString()} matching`;
      } else {
        selAllMatchBtn.hidden = true;
      }
    }

    const hasArrFix = selectedGroups.some((g) => (g.arr_matches || []).some((m) => m.can_import || m.can_rescan));
    const hasTorrents = selectedGroups.some((g) => g.torrents.length > 0);
    const hasHardlinkPairs = selectedGroups.some((g) => g.can_hardlink && g.hardlink_pairs.length > 0);
    const hasFiles = selectedGroups.some((g) => g.all_paths.length > 0);

    $("bulkArrFixBtn").disabled = !hasArrFix;
    $("bulkDeleteTorrentsBtn").disabled = !hasTorrents;
    $("bulkHardlinkBtn").disabled = !hasHardlinkPairs;
    $("bulkDeleteFilesBtn").disabled = !hasFiles;
  }

  function getSelectedGroups() {
    if (!state.scan || !state.scan.groups) return [];
    return state.scan.groups.filter((g) => state.selectedGroupIds.has(g.id));
  }

  function renderAll() {
    renderHeaderReadOnly();
    renderTargetsStrip();
    renderAlerts();
    renderSummary();
    renderTable();
  }

  async function loadConfig() {
    state.config = await apiFetch("/api/config");
    renderAll();
  }

  async function runScan() {
    const btn = $("scanNowBtn");
    const spinner = $("scanSpinner");
    const text = $("scanBtnText");
    btn.disabled = true;
    spinner.hidden = false;
    text.textContent = "Scanning...";
    state.scanEpoch += 1;
    state.pendingArrRefreshGroups.clear();
    try {
      state.scan = await apiFetch("/api/scan", { method: "POST" });
      state.page = 1;
      state.selectedGroupIds.clear();
      state.expandedPackIds.clear();
      state.expandedGroupIds.clear();
      state.currentPackMap.clear();
      renderAll();
      showToast(
        `Scan complete: ${state.scan.summary.total_files_scanned} files analyzed (${formatBytes(state.scan.summary.total_reclaimable_bytes)} reclaimable).`,
        "success"
      );
    } catch (err) {
      showToast(`Scan failed: ${err.message}`, "error");
    } finally {
      btn.disabled = false;
      spinner.hidden = true;
      text.textContent = "Scan Now";
    }
  }

  function expandAffectedGroupIds(actionType, groups, payload) {
    const ids = new Set((groups || []).map((g) => g.id).filter(Boolean));
    if (actionType === "delete-torrents" && state.scan && state.scan.groups && payload && payload.items) {
      const deletedHashes = new Set(
        payload.items.map((it) => String(it.torrent_hash || "").toLowerCase()).filter(Boolean)
      );
      for (const g of state.scan.groups) {
        if ((g.torrents || []).some((t) => deletedHashes.has(String(t.torrent_hash || "").toLowerCase()))) {
          ids.add(g.id);
        }
      }
    }
    return [...ids];
  }

  function collectWaitDeletedPaths(actionType, groups, payload) {
    if (actionType !== "delete-torrents" || !payload || !payload.delete_files) {
      return [];
    }
    const deletedHashes = new Set(
      (payload.items || []).map((it) => String(it.torrent_hash || "").toLowerCase()).filter(Boolean)
    );
    const paths = new Set();
    for (const g of groups || []) {
      for (const t of g.torrents || []) {
        if (!deletedHashes.has(String(t.torrent_hash || "").toLowerCase())) continue;
        if (t.mapped_path) {
          paths.add(t.mapped_path);
        }
        if (t.match_type === "path" && Array.isArray(t.matched_paths)) {
          for (const mp of t.matched_paths) {
            if (mp) paths.add(mp);
          }
        }
      }
    }
    return [...paths];
  }

  function findPackSiblings(baseGroups) {
    if (!state.scan || !Array.isArray(state.scan.groups)) {
      return { importableSiblings: [], otherSiblings: [] };
    }
    const baseIds = new Set((baseGroups || []).map((g) => g.id));
    const multiTorrentKeys = new Set();
    for (const g of baseGroups || []) {
      for (const t of g.torrents || []) {
        if ((t.torrent_file_count || 0) > 1 && t.instance_id && t.torrent_hash) {
          multiTorrentKeys.add(`${t.instance_id}::${String(t.torrent_hash).toLowerCase()}`);
        }
      }
    }
    if (multiTorrentKeys.size === 0) {
      return { importableSiblings: [], otherSiblings: [] };
    }
    const importableSiblings = [];
    const otherSiblings = [];
    for (const g of state.scan.groups) {
      if (baseIds.has(g.id)) continue;
      const sharesMulti = (g.torrents || []).some(
        (t) =>
          (t.torrent_file_count || 0) > 1 &&
          multiTorrentKeys.has(`${t.instance_id}::${String(t.torrent_hash || "").toLowerCase()}`)
      );
      if (!sharesMulti) continue;
      if ((g.arr_matches || []).some((m) => m.can_import)) {
        importableSiblings.push(g);
      } else {
        otherSiblings.push(g);
      }
    }
    return { importableSiblings, otherSiblings };
  }

  function updatePackSiblingsOptionUi(type, baseGroups, mode) {
    const row = $("packSiblingsOptionRow");
    const chk = $("includePackSiblingsCheckbox");
    const lbl = $("packSiblingsLabel");
    const hint = $("packSiblingsHint");
    if (!row || !chk || !lbl || !hint) return { importableSiblings: [], otherSiblings: [] };

    const isArrImport = type === "arr-import" || type === "arr-fix";
    const info = findPackSiblings(baseGroups);
    if (!isArrImport || mode !== "move" || info.importableSiblings.length === 0) {
      row.hidden = true;
      return info;
    }

    row.hidden = false;
    lbl.textContent = `Also Move ${info.importableSiblings.length} sibling episode(s) from the same multi-file torrent pack so qBittorrent can relocate cleanly`;
    const sampleNames = info.importableSiblings
      .slice(0, 3)
      .map((g) => g.filename)
      .join(", ");
    const extraMore = info.importableSiblings.length > 3 ? ` (+${info.importableSiblings.length - 3} more)` : "";
    const otherNote = info.otherSiblings.length > 0
      ? ` • ${info.otherSiblings.length} sibling file(s) already imported or replaced in Arr`
      : "";
    hint.textContent = `Siblings: ${sampleNames}${extraMore}${otherNote}`;
    return info;
  }

  function getEffectiveActionGroups(pendingAction) {
    if (!pendingAction) return [];
    const baseGroups = pendingAction.baseGroups || pendingAction.groups || [];
    const type = pendingAction.type;
    if (type !== "arr-import" && type !== "arr-fix") return baseGroups;
    const mode = ($("arrImportModeSelect") && $("arrImportModeSelect").value) || "auto";
    if (mode !== "move") return baseGroups;
    const sibChk = $("includePackSiblingsCheckbox");
    if (!sibChk || !sibChk.checked) return baseGroups;
    const { importableSiblings } = findPackSiblings(baseGroups);
    if (importableSiblings.length === 0) return baseGroups;
    return [...baseGroups, ...importableSiblings];
  }

  function collectInspectInput(groups, importItems) {
    const seenTorrents = new Set();
    const torrents = [];
    for (const g of groups || []) {
      for (const t of g.torrents || []) {
        const hk = `${t.instance_id}::${String(t.torrent_hash || "").toLowerCase()}`;
        if (!t.instance_id || !t.torrent_hash || seenTorrents.has(hk)) continue;
        seenTorrents.add(hk);
        torrents.push({ instance_id: t.instance_id, torrent_hash: t.torrent_hash });
      }
    }

    const pathSet = new Set();
    for (const it of importItems || []) {
      if (it.local_path) pathSet.add(it.local_path);
    }
    for (const g of groups || []) {
      if (!(g.arr_matches || []).some((m) => m.can_import)) continue;
      for (const p of g.all_paths || []) {
        if (p) pathSet.add(p);
      }
    }
    return { torrents, selected_local_paths: [...pathSet] };
  }

  function buildQbitRelocatePreview(groups, inspectResult = null) {
    if (inspectResult && Array.isArray(inspectResult.torrents)) {
      const eligible = [];
      const blocked = [];
      for (const t of inspectResult.torrents) {
        if (t.can_relocate) eligible.push(t);
        else blocked.push(t);
      }
      return {
        fromInspect: true,
        eligible,
        blocked,
        errors: inspectResult.errors || [],
      };
    }

    const eligible = [];
    const blocked = [];
    const seen = new Set();
    for (const g of groups || []) {
      for (const t of g.torrents || []) {
        const k = `${t.instance_id}::${String(t.torrent_hash || "").toLowerCase()}`;
        if (seen.has(k)) continue;
        seen.add(k);
        const item = {
          instance_id: t.instance_id,
          instance_name: t.instance_name,
          torrent_hash: t.torrent_hash,
          torrent_name: t.torrent_name,
          file_count: t.torrent_file_count || 0,
          companion_count: 0,
          sample_count: 0,
          can_relocate: (t.torrent_file_count || 0) === 1,
          status: (t.torrent_file_count || 0) === 1 ? "ready" : "pending_inspect",
          warnings: [],
          files: [],
        };
        if (item.can_relocate) eligible.push(item);
        else blocked.push(item);
      }
    }
    return { fromInspect: false, eligible, blocked, errors: [] };
  }

  async function refreshGroupsInPlace(groupIds, waitDeletedPaths = [], includeArr = true, extraQbitHashes = null) {
    const uniqueIds = [...new Set((groupIds || []).filter(Boolean))];
    if (!state.scan || uniqueIds.length === 0) return null;

    const epoch = state.scanEpoch;
    const bodyObj = {
      group_ids: uniqueIds,
      wait_for_deleted_paths: waitDeletedPaths,
      include_arr: includeArr,
    };
    if (extraQbitHashes && Object.keys(extraQbitHashes).length > 0) {
      bodyObj.extra_qbit_hashes = extraQbitHashes;
    }

    const res = await apiFetch("/api/scan/refresh-groups", {
      method: "POST",
      body: JSON.stringify(bodyObj),
    });
    if (epoch !== state.scanEpoch) return null;

    if (res.full_scan) {
      state.scan = res.full_scan;
      state.page = 1;
      state.selectedGroupIds.clear();
      state.expandedPackIds.clear();
      state.expandedGroupIds.clear();
      state.currentPackMap.clear();
      state.pendingArrRefreshGroups.clear();
      renderAll();
      return res;
    }

    const removedSet = new Set(res.removed_group_ids || []);
    const updatedMap = new Map((res.updated_groups || []).map((g) => [g.id, g]));

    for (const id of removedSet) {
      state.selectedGroupIds.delete(id);
      state.expandedGroupIds.delete(id);
      state.pendingArrRefreshGroups.delete(id);
    }

    state.scan.groups = state.scan.groups
      .filter((g) => !removedSet.has(g.id))
      .map((g) => updatedMap.get(g.id) || g);

    if (res.summary) {
      state.scan.summary = res.summary;
    }
    renderTargetsStrip();
    renderSummary();
    renderTable();
    return res;
  }

  async function pollArrCommandsAndRefresh(commands, groupIds, relocatePlan = null) {
    const uniqueIds = [...new Set((groupIds || []).filter(Boolean))];
    if (uniqueIds.length === 0) return;

    const epoch = state.scanEpoch;
    const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
    let timedOut = false;

    try {
      if (commands && commands.length > 0) {
        const maxAttempts = 40; // ~60s total at 1.5s intervals
        let allDone = false;
        for (let attempt = 0; attempt < maxAttempts; attempt++) {
          await sleep(1500);
          if (epoch !== state.scanEpoch) return;

          const statusRes = await apiFetch("/api/arr/commands/status", {
            method: "POST",
            body: JSON.stringify({ commands }),
          });
          if (epoch !== state.scanEpoch) return;
          if (statusRes.all_done) {
            allDone = true;
            break;
          }
        }
        timedOut = !allDone;
      }

      if (epoch !== state.scanEpoch) return;
      for (const id of uniqueIds) {
        state.pendingArrRefreshGroups.delete(id);
      }
      const refRes = await refreshGroupsInPlace(uniqueIds, [], true);
      if (epoch !== state.scanEpoch) return;

      if (relocatePlan && relocatePlan.inspectedTorrents && relocatePlan.inspectedTorrents.length > 0 && refRes && !refRes.full_scan) {
        const newPathByInode = new Map();
        const newPathByOldPath = new Map();

        for (const updatedGrp of refRes.updated_groups || []) {
          const pre = relocatePlan.groupPreMoveMap.get(updatedGrp.id);
          const diskSet = new Set((updatedGrp.all_paths || []).map((p) => String(p).toLowerCase()));
          const imported = (updatedGrp.arr_matches || []).find(
            (m) => m.match_kind === "imported" && m.file_path && diskSet.has(String(m.file_path).toLowerCase())
          );
          const candidatePath =
            (imported && imported.file_path) ||
            (updatedGrp.folder_a_files && updatedGrp.folder_a_files[0] && updatedGrp.folder_a_files[0].abs_path) ||
            (updatedGrp.all_paths && updatedGrp.all_paths[0]) ||
            "";

          if (!candidatePath) continue;
          if (pre && pre.oldPaths.has(String(candidatePath).toLowerCase())) {
            continue;
          }

          for (const f of [...(updatedGrp.folder_a_files || []), ...(updatedGrp.folder_b_files || [])]) {
            const ino = f.inode_str || (f.inode ? String(f.inode) : "");
            if (ino && ino !== "0") {
              newPathByInode.set(`${f.device || 0}:${ino}`, candidatePath);
            }
          }
          if (pre) {
            for (const ik of pre.inodeKeys || []) {
              newPathByInode.set(ik, candidatePath);
            }
            for (const oldP of pre.oldPaths || []) {
              newPathByOldPath.set(oldP, candidatePath);
            }
          }
        }

        const relocateItems = [];
        const extraHashesByInst = {};
        const seenHash = new Set();

        for (const t of relocatePlan.inspectedTorrents) {
          const hk = `${t.instance_id}::${String(t.torrent_hash || "").toLowerCase()}`;
          if (seenHash.has(hk)) continue;

          const fileMappings = [];
          let movedFileCount = 0;
          for (const tf of t.files || []) {
            let newLocal = "";
            const inoKey = tf.inode_str ? `${tf.device || 0}:${tf.inode_str}` : "";
            if (inoKey && newPathByInode.has(inoKey)) {
              newLocal = newPathByInode.get(inoKey);
            } else if (tf.local_path && newPathByOldPath.has(String(tf.local_path).toLowerCase())) {
              newLocal = newPathByOldPath.get(String(tf.local_path).toLowerCase());
            }

            if (newLocal && (!tf.local_path || newLocal.toLowerCase() !== String(tf.local_path).toLowerCase())) {
              movedFileCount++;
            }
            fileMappings.push({
              old_rel_path: tf.rel_path,
              old_local_path: tf.local_path || "",
              new_local_path: newLocal,
              kind: tf.kind || tf.raw_kind || "",
              device: tf.device || 0,
              inode_str: tf.inode_str || "",
              size: tf.size || 0,
            });
          }

          if (movedFileCount === 0) continue;
          seenHash.add(hk);
          relocateItems.push({
            instance_id: t.instance_id,
            torrent_hash: t.torrent_hash,
            file_mappings: fileMappings,
            move_companions: relocatePlan.moveCompanions,
            move_samples: relocatePlan.moveSamples,
          });
          if (!extraHashesByInst[t.instance_id]) extraHashesByInst[t.instance_id] = [];
          extraHashesByInst[t.instance_id].push(t.torrent_hash);
        }

        if (relocateItems.length > 0) {
          const relRes = await apiFetch("/api/actions/qbit-relocate", {
            method: "POST",
            body: JSON.stringify({ items: relocateItems, dry_run: false }),
          });
          if (epoch !== state.scanEpoch) return;
          await refreshGroupsInPlace(uniqueIds, [], true, extraHashesByInst);
          const relErrs = relRes.errors || [];
          const renamedTotal = (relRes.results || []).reduce(
            (acc, r) => acc + (r.renamed_count != null ? r.renamed_count : r.renamed ? 1 : 0),
            0
          );
          const companionMoved = (relRes.results || []).reduce(
            (acc, r) => acc + ((r.moved_companions && r.moved_companions.length) || 0),
            0
          );
          const extraParts = [];
          if (renamedTotal > 0) extraParts.push(`${renamedTotal} file path(s) updated`);
          if (companionMoved > 0) extraParts.push(`${companionMoved} .nfo/.srt moved`);
          const extraStr = extraParts.length > 0 ? ` (${extraParts.join(", ")})` : "";
          const errStr =
            relErrs.length > 0
              ? ` • ${relErrs.length} error/skipped${relErrs[0] && relErrs[0].error ? `: ${relErrs[0].error}` : ""}`
              : "";
          showToast(
            `Arr sync done & relocated ${relRes.relocated_count} torrent(s) in qBittorrent${extraStr}${errStr}.`,
            relErrs.length > 0 && relRes.relocated_count === 0 ? "error" : "success"
          );
          return;
        }
      }

      showToast(
        timedOut
          ? "Radarr/Sonarr sync still running — refreshed current status."
          : "Radarr/Sonarr sync completed; item status updated.",
        "success"
      );
    } catch (err) {
      if (epoch !== state.scanEpoch) return;
      for (const id of uniqueIds) {
        state.pendingArrRefreshGroups.delete(id);
      }
      renderTable();
      showToast(`Radarr/Sonarr sync check failed: ${err.message}`, "error");
    }
  }

  async function syncSurvivingGroupsWithArr(groupIds) {
    const uniqueIds = [...new Set((groupIds || []).filter(Boolean))];
    if (uniqueIds.length === 0 || !state.scan) return;

    const epoch = state.scanEpoch;
    for (const id of uniqueIds) {
      state.pendingArrRefreshGroups.add(id);
    }
    renderTable();

    try {
      await refreshGroupsInPlace(uniqueIds, [], true);
    } catch (err) {
      if (epoch === state.scanEpoch) {
        showToast(`Radarr/Sonarr sync failed: ${err.message}`, "error");
      }
    } finally {
      if (epoch === state.scanEpoch) {
        for (const id of uniqueIds) {
          state.pendingArrRefreshGroups.delete(id);
        }
        renderTable();
      }
    }
  }

  async function toggleReadOnly() {
    try {
      const next = !state.config.read_only_mode;
      state.config = await apiFetch("/api/config/read-only", {
        method: "PATCH",
        body: JSON.stringify({ read_only_mode: next }),
      });
      renderHeaderReadOnly();
      showToast(
        next ? "Safe Read-Only Mode enabled." : "Write Mode enabled — cleanup actions are now unlocked.",
        next ? "info" : "success"
      );
    } catch (err) {
      showToast(`Could not toggle Read-Only Mode: ${err.message}`, "error");
    }
  }

  function syncActiveTabUi() {
    document.querySelectorAll("#quickTabs .tab-btn").forEach((btn) => {
      btn.classList.toggle("active", btn.dataset.tab === state.activeTab);
    });
  }

  // --- Action Preview & Execution Modal ---
  function buildActionPayloadForGroups(actionType, groups) {
    if (actionType === "convert-hardlinks") {
      const pairs = [];
      for (const g of groups) {
        if (g.can_hardlink && g.hardlink_pairs) {
          pairs.push(...g.hardlink_pairs);
        }
      }
      return { pairs };
    }

    if (actionType === "delete-torrents") {
      const seen = new Set();
      const items = [];
      for (const g of groups) {
        for (const t of g.torrents) {
          const key = `${t.instance_id}::${t.torrent_hash}`;
          if (seen.has(key)) continue;
          seen.add(key);
          items.push({
            instance_id: t.instance_id,
            torrent_hash: t.torrent_hash,
            torrent_name: t.torrent_name,
            instance_name: t.instance_name,
            expected_paths: t.matched_paths || [],
          });
        }
      }
      return { items, delete_files: $("deleteTorrentFilesCheckbox").checked };
    }

    if (actionType === "delete-files") {
      const filePaths = [];
      for (const g of groups) {
        if (g.status === "ORPHANED_IN_B") {
          filePaths.push(...g.folder_b_files.map((f) => f.abs_path));
        } else {
          filePaths.push(...g.all_paths);
        }
      }
      return { file_paths: [...new Set(filePaths)], cleanup_empty_dirs: true };
    }

    if (actionType === "arr-import" || actionType === "arr-rescan" || actionType === "arr-fix") {
      const importSeen = new Set();
      const importItems = [];
      const rescanSeen = new Set();
      const rescanItems = [];

      for (const g of groups) {
        const dlHash = (g.torrents && g.torrents[0] && g.torrents[0].torrent_hash) || "";
        const fallbackLocal = (g.folder_b_files && g.folder_b_files[0] && g.folder_b_files[0].abs_path) || (g.all_paths && g.all_paths[0]) || "";
        for (const m of g.arr_matches || []) {
          if ((actionType === "arr-import" || actionType === "arr-fix") && m.can_import) {
            const localPath = m.file_path || fallbackLocal;
            const ik = `${m.instance_id}::${localPath}`;
            if (localPath && !importSeen.has(ik)) {
              importSeen.add(ik);
              importItems.push({
                instance_id: m.instance_id,
                local_path: localPath,
                download_client_id: dlHash,
                item_id: m.item_id || 0,
                episode_ids: m.episode_ids || [],
                target_root_path: m.root_path || "",
              });
            }
          }
          if ((actionType === "arr-rescan" || actionType === "arr-fix") && m.can_rescan && m.item_id > 0) {
            const rk = `${m.instance_id}::${m.item_id}`;
            if (!rescanSeen.has(rk)) {
              rescanSeen.add(rk);
              rescanItems.push({
                instance_id: m.instance_id,
                item_id: m.item_id,
                title: renderArrTitle(m),
              });
            }
          }
        }
      }

      return {
        importItems,
        rescanItems,
        force_manual_import: $("forceManualImportCheckbox").checked,
        import_mode: ($("arrImportModeSelect") && $("arrImportModeSelect").value) || "auto",
      };
    }

    return {};
  }

  function renderActionExplanation(type, groups, payload, options = {}, inspectResult = null) {
    const explainEl = $("actionExplainBox");
    const warnEl = $("actionContextWarnings");
    if (!explainEl || !warnEl) return;

    const primaryPaths = new Set();
    for (const g of groups || []) {
      for (const f of g.folder_a_files || []) {
        if (f.abs_path) primaryPaths.add(f.abs_path.toLowerCase());
      }
    }

    let title = "What this action will do";
    const bullets = [];
    const warnings = [];

    if (type === "convert-hardlinks") {
      title = "What happens when you Convert Copies \u2192 Hardlinks";
      bullets.push(
        "Verifies byte-for-byte identity (SHA-256 head, middle, and tail samples) before changing any file.",
        "Replaces duplicate copies atomically with filesystem hardlinks sharing the primary file\u2019s inode.",
        "Keeps all filenames and folder paths unchanged and leaves qBittorrent seeding uninterrupted."
      );
    } else if (type === "delete-torrents") {
      title = options.erroredOnly
        ? "What happens when you Delete Errored / Unregistered Torrents"
        : "What happens when you Delete Torrents in qBittorrent";
      if (payload.delete_files) {
        bullets.push(
          "Removes the listed torrent(s) from qBittorrent AND deletes data files at qBittorrent\u2019s save path.",
          "Hardlinked copies in other folders (such as Plex or cross-seeds) keep their own directory entry; physical disk space is freed only if no other hardlink remains."
        );
      } else {
        bullets.push(
          "Removes the listed torrent(s) from qBittorrent only (stops tracking/seeding in client).",
          "Leaves all media files on disk completely untouched in all folders."
        );
      }
      if (options.erroredOnly) {
        bullets.push("Targets only errored / unregistered torrents; healthy seeding torrents stay untouched.");
      }

      if (payload.delete_files) {
        const hitsPrimary = (payload.items || []).some((it) =>
          (it.expected_paths || []).some((p) => primaryPaths.has(String(p).toLowerCase()))
        );
        if (hitsPrimary) {
          warnings.push(
            "Warning: At least one torrent\u2019s save path points inside a Primary (Folder A / Plex) folder. Deleting torrent data will delete that Primary file."
          );
        }
      }
      if (!options.erroredOnly) {
        const healthyKeys = new Set();
        for (const g of groups || []) {
          for (const t of g.torrents || []) {
            if (t.is_seeding && !t.tracker_health) {
              healthyKeys.add(`${t.instance_id}::${t.torrent_hash}`);
            }
          }
        }
        if (healthyKeys.size > 0) {
          warnings.push(
            `Warning: ${healthyKeys.size} selected torrent(s) are currently seeding cleanly with no tracker errors.`
          );
        }
      }
    } else if (type === "delete-files") {
      title = "What happens when you Delete Files from Disk";
      bullets.push(
        "Permanently unlinks/deletes the listed file paths from disk and removes empty parent subdirectories inside configured folders.",
        "Does NOT remove torrents from qBittorrent (and does not directly edit Radarr/Sonarr)."
      );
      if ((groups || []).some((g) => (g.torrents || []).length > 0)) {
        warnings.push(
          "Warning: Some selected items are still tracked in qBittorrent. Deleting files directly from disk will cause Missing Files errors in qBittorrent \u2014 use Delete Torrent instead if you want to remove the torrent."
        );
      }
      if ((payload.file_paths || []).some((p) => primaryPaths.has(String(p).toLowerCase()))) {
        warnings.push("Warning: One or more files to be deleted are inside a Primary (Folder A / Plex) folder.");
      }
      if ((groups || []).some((g) => g.status === "ORPHANED_IN_B" && g.external_hardlink_count > 0)) {
        warnings.push(
          "Note: External hardlinks still exist elsewhere on disk for at least one item, so removing these paths will not free physical disk space."
        );
      }
    } else if (type === "arr-rescan") {
      title = "What happens when you Rescan in Radarr / Sonarr";
      bullets.push(
        "Triggers RefreshMovie / RescanSeries in Radarr or Sonarr so it scans the item\u2019s existing library folder on disk.",
        "Links unlinked files already inside Arr\u2019s library folder without moving, renaming, or deleting any files.",
        "Does NOT touch qBittorrent."
      );
    } else if (type === "arr-import" || type === "arr-fix") {
      title =
        type === "arr-fix"
          ? "What happens when you Fix in Radarr / Sonarr"
          : "What happens when you Import into Radarr / Sonarr";
      const importCount = (payload.importItems || []).length;
      const rescanCount = (payload.rescanItems || []).length;
      if (importCount > 0) {
        if (payload.force_manual_import) {
          bullets.push(
            "Import Method: Force Manual Import \u2014 instructs Arr to import matched files directly using their matched movie/episode IDs, bypassing automatic rejections."
          );
        } else {
          bullets.push(
            "Import Method: DownloadedMoviesScan / DownloadedEpisodesScan \u2014 asks Radarr/Sonarr to identify the release and import it into its configured movie/series library folder shown below."
          );
        }
        if (payload.import_mode === "copy") {
          bullets.push(
            "Import Mode: Copy / Hardlink \u2014 keeps the original file in place (creating a hardlink on the same filesystem, or copying otherwise). Recommended while seeding or importing from a Primary folder."
          );
        } else if (payload.import_mode === "move") {
          bullets.push(
            "Import Mode: Move \u2014 relocates/renames the file into Radarr/Sonarr\u2019s library folder and removes the original source file path."
          );
        } else {
          bullets.push(
            "Import Mode: Auto \u2014 Arr decides: uses hardlink/copy for tracked download client items, or may MOVE/rename untracked files out of their current folder."
          );
        }
      }
      if (rescanCount > 0) {
        bullets.push(
          "Rescan items: triggers RefreshMovie / RescanSeries to index files already sitting in Arr\u2019s library folder without moving or deleting anything."
        );
      }
      bullets.push(
        "Does NOT remove or modify torrents in qBittorrent. Runs asynchronously in Arr while SeedLink monitors command completion."
      );

      if (importCount > 0) {
        const hasTorrents = (groups || []).some((g) => (g.torrents || []).length > 0);
        const hitsPrimarySource = (payload.importItems || []).some((it) =>
          primaryPaths.has(String(it.local_path || "").toLowerCase())
        );
        const relocateInfo = buildQbitRelocatePreview(groups, inspectResult);
        const relocateRow = $("qbitRelocateOptionRow");
        const relocateCheck = $("relocateQbitAfterMoveCheckbox");
        const moveCompanions = $("moveCompanionFilesCheckbox") ? $("moveCompanionFilesCheckbox").checked : true;
        const moveSamples = $("moveSampleFilesCheckbox") ? $("moveSampleFilesCheckbox").checked : false;
        const relocateActive =
          payload.import_mode === "move" &&
          relocateRow &&
          !relocateRow.hidden &&
          relocateCheck &&
          relocateCheck.checked &&
          relocateInfo.eligible.length > 0;

        if (payload.import_mode === "move" && relocateActive) {
          const singleCnt = relocateInfo.eligible.filter((t) => t.file_count === 1).length;
          const packCnt = relocateInfo.eligible.length - singleCnt;
          const kindDesc = packCnt > 0 && singleCnt > 0
            ? `${packCnt} pack(s) and ${singleCnt} single-file torrent(s)`
            : packCnt > 0
              ? `${packCnt} multi-file torrent pack(s)`
              : `${singleCnt} single-file torrent(s)`;
          bullets.push(
            `After Arr finishes moving, SeedLink will stop, disable Auto TMM, update location & per-file names, recheck, and resume ${kindDesc} in qBittorrent.`
          );

          const totalCompanions = relocateInfo.eligible.reduce((acc, t) => acc + (t.companion_count || 0), 0);
          if (totalCompanions > 0) {
            if (moveCompanions) {
              bullets.push(
                `${totalCompanions} companion file(s) (.nfo, .srt, etc.) will also be relocated alongside the pack (if left behind by Arr) so qBittorrent keeps 100% seeding.`
              );
            } else {
              warnings.push(
                `Warning: ${totalCompanions} companion file(s) (.nfo, .srt) will NOT be moved automatically. If they sit in a different folder than the moved videos, pack relocation will be blocked to avoid partial/99% seeding.`
              );
            }
          }
          const totalSamples = relocateInfo.eligible.reduce((acc, t) => acc + (t.sample_count || 0), 0);
          if (totalSamples > 0) {
            if (moveSamples) {
              warnings.push(
                `Note: ${totalSamples} Sample/Extra file(s) are included and will move into your target library folder.`
              );
            } else {
              warnings.push(
                `Warning: ${totalSamples} Sample/Extra file(s) detected and excluded by default. If they live in Downloads while videos move to Plex, qBittorrent auto-relocate will skip unless you enable moving them or switch to Copy / Hardlink.`
              );
            }
          }
          if (relocateInfo.blocked.length > 0) {
            const partialCnt = relocateInfo.blocked.filter((t) => t.status === "partial_pack").length;
            if (partialCnt > 0) {
              warnings.push(
                `Warning: ${partialCnt} torrent(s) are partial packs (unselected video episodes remain) and will NOT be auto-relocated in qBittorrent.`
              );
            } else {
              warnings.push(
                `Warning: ${relocateInfo.blocked.length} matched torrent(s) cannot be auto-relocated and will be skipped.`
              );
            }
          }
        } else if (payload.import_mode !== "copy" && hasTorrents) {
          warnings.push(
            `Warning: Selected item(s) are tracked/seeding in qBittorrent. With Import Mode "${payload.import_mode === "move" ? "Move" : "Auto"}", Radarr/Sonarr can rename/relocate the file into its target folder and break seeding unless relocated (or switch Import Mode to "Copy / Hardlink").`
          );
        }
        if (hitsPrimarySource) {
          if (payload.import_mode !== "copy") {
            if (!relocateActive) {
              warnings.push(
                "Warning: The source file is already in a Primary (Plex) folder. Moving it may relocate it into Radarr/Sonarr\u2019s target folder (shown below) and break seeding if qBittorrent points here."
              );
            } else {
              warnings.push(
                "Note: The source file is inside a Primary (Plex) folder; Arr will move it to the target library folder below and SeedLink will update qBittorrent to seed from that new path."
              );
            }
          } else {
            warnings.push(
              "Note: Source file sits in a Primary (Plex) folder. Copy / Hardlink will keep this original path intact while linking/copying to Arr\u2019s target library folder shown below."
            );
          }
        }
      }
    }

    explainEl.innerHTML = `
      <div class="action-explain-title">${escapeHtml(title)}</div>
      <ul class="action-explain-list">${bullets.map((b) => `<li>${escapeHtml(b)}</li>`).join("")}</ul>
    `;

    if (warnings.length > 0) {
      warnEl.hidden = false;
      warnEl.innerHTML = warnings.map((w) => `<div>${escapeHtml(w)}</div>`).join("");
    } else {
      warnEl.hidden = true;
      warnEl.innerHTML = "";
    }
  }

  function updateQbitRelocateOptionUi(type, groups, payload, inspectResult = null) {
    const row = $("qbitRelocateOptionRow");
    const chk = $("relocateQbitAfterMoveCheckbox");
    const hint = $("qbitRelocateSummary");
    const compRow = $("qbitCompanionOptionRow");
    const compLbl = $("moveCompanionFilesLabel");
    const sampleRow = $("qbitSampleOptionRow");
    const sampleLbl = $("moveSampleFilesLabel");
    if (!row || !chk || !hint) return;

    const isArrImport = (type === "arr-import" || type === "arr-fix") && payload.importItems && payload.importItems.length > 0;
    const relocateInfo = buildQbitRelocatePreview(groups, inspectResult);
    const hasAnyTorrents = relocateInfo.eligible.length + relocateInfo.blocked.length > 0;

    if (!isArrImport || payload.import_mode !== "move" || (!hasAnyTorrents && relocateInfo.errors.length === 0)) {
      row.hidden = true;
      if (compRow) compRow.hidden = true;
      if (sampleRow) sampleRow.hidden = true;
      return;
    }

    row.hidden = false;
    if (relocateInfo.eligible.length === 0) {
      chk.dataset.wasDisabled = "true";
      chk.checked = false;
      chk.disabled = true;
      if (compRow) compRow.hidden = true;
      if (sampleRow) sampleRow.hidden = true;

      if (relocateInfo.blocked.length > 0) {
        const firstWarn = relocateInfo.blocked.find((b) => b.warnings && b.warnings.length > 0);
        hint.textContent = firstWarn
          ? firstWarn.warnings[0]
          : `${relocateInfo.blocked.length} matched torrent(s) cannot be auto-relocated.`;
      } else if (relocateInfo.errors.length > 0) {
        hint.textContent = `Could not inspect torrent(s): ${relocateInfo.errors[0].error || "unknown error"}`;
      } else {
        hint.textContent = "No eligible torrents found to relocate.";
      }
      return;
    }

    if (chk.dataset.wasDisabled === "true") {
      chk.checked = true;
      chk.dataset.wasDisabled = "false";
    }
    chk.disabled = false;

    const singleCnt = relocateInfo.eligible.filter((t) => t.file_count === 1).length;
    const packCnt = relocateInfo.eligible.length - singleCnt;
    const readyParts = [];
    if (singleCnt > 0) readyParts.push(`${singleCnt} single-file`);
    if (packCnt > 0) readyParts.push(`${packCnt} multi-file pack`);
    const parts = [`${relocateInfo.eligible.length} torrent(s) ready to relocate & recheck (${readyParts.join(", ")})`];
    if (relocateInfo.blocked.length > 0) {
      parts.push(`${relocateInfo.blocked.length} blocked/skipped`);
    }
    hint.textContent = parts.join("; ") + ".";

    const companionNames = [];
    let companionCount = 0;
    const sampleNames = [];
    let sampleCount = 0;
    for (const t of relocateInfo.eligible) {
      for (const f of t.files || []) {
        if (f.kind === "metadata" || f.kind === "subtitle") {
          companionCount++;
          if (companionNames.length < 4) companionNames.push(f.filename);
        } else if (f.kind === "sample") {
          sampleCount++;
          if (sampleNames.length < 3) sampleNames.push(f.rel_path);
        }
      }
    }

    if (compRow && compLbl) {
      if (companionCount > 0 && chk.checked) {
        compRow.hidden = false;
        const more = companionCount > companionNames.length ? ` (+${companionCount - companionNames.length} more)` : "";
        compLbl.textContent = `Also move ${companionCount} companion file(s) (${companionNames.join(", ")}${more}) with the pack so qBittorrent stays 100%`;
      } else {
        compRow.hidden = true;
      }
    }
    if (sampleRow && sampleLbl) {
      if (sampleCount > 0 && chk.checked) {
        sampleRow.hidden = false;
        const more = sampleCount > sampleNames.length ? ` (+${sampleCount - sampleNames.length} more)` : "";
        sampleLbl.textContent = `Also move ${sampleCount} Sample / Extra file(s) (${sampleNames.join(", ")}${more}) — warning: may show up in Plex library`;
      } else {
        sampleRow.hidden = true;
      }
    }
  }

  async function openActionModal(actionType, groups, options = {}) {
    state.pendingAction = { type: actionType, baseGroups: groups, groups, options };
    state.lastInspectResult = null;
    const titles = {
      "convert-hardlinks": "Convert Duplicate Copies to Hardlinks",
      "delete-torrents": options.erroredOnly ? "Delete Errored Torrents from qBittorrent" : "Delete Torrents from qBittorrent",
      "delete-files": "Delete Files from Disk",
      "arr-import": "Import Release into Radarr / Sonarr",
      "arr-rescan": "Rescan Library Item in Radarr / Sonarr",
      "arr-fix": "Fix Selected References in Radarr / Sonarr",
    };
    $("actionModalTitle").textContent = `${titles[actionType]} — Dry-Run Preview`;
    $("torrentDeleteOptionRow").hidden = actionType !== "delete-torrents";
    $("forceManualImportCheckbox").checked = false;
    if ($("includePackSiblingsCheckbox")) $("includePackSiblingsCheckbox").checked = true;
    if ($("packSiblingsOptionRow")) $("packSiblingsOptionRow").hidden = true;
    $("relocateQbitAfterMoveCheckbox").checked = true;
    $("relocateQbitAfterMoveCheckbox").disabled = false;
    $("relocateQbitAfterMoveCheckbox").dataset.wasDisabled = "false";
    if ($("moveCompanionFilesCheckbox")) $("moveCompanionFilesCheckbox").checked = true;
    if ($("moveSampleFilesCheckbox")) $("moveSampleFilesCheckbox").checked = false;
    if ($("qbitCompanionOptionRow")) $("qbitCompanionOptionRow").hidden = true;
    if ($("qbitSampleOptionRow")) $("qbitSampleOptionRow").hidden = true;

    const arrPayload = (actionType === "arr-import" || actionType === "arr-fix")
      ? buildActionPayloadForGroups(actionType, groups)
      : null;
    const hasArrImports = Boolean(arrPayload && arrPayload.importItems.length > 0);
    $("arrImportOptionRow").hidden = !hasArrImports;
    if (hasArrImports) {
      const hasTorrentsOrPrimary = (groups || []).some(
        (g) => (g.torrents && g.torrents.length > 0) || (g.folder_a_files && g.folder_a_files.length > 0)
      );
      $("arrImportModeSelect").value = hasTorrentsOrPrimary ? "copy" : "auto";
    }
    const isArrAction = actionType.startsWith("arr-");
    $("confirmActionBtn").className = isArrAction ? "btn btn-primary" : "btn btn-danger";
    $("actionModal").hidden = false;
    await refreshActionDryRun();
  }

  async function refreshActionDryRun() {
    if (!state.pendingAction) return;
    const previewEpoch = ++state.actionPreviewEpoch;
    const { type, options } = state.pendingAction;
    const baseGroups = state.pendingAction.baseGroups || state.pendingAction.groups || [];
    const currentMode = ($("arrImportModeSelect") && $("arrImportModeSelect").value) || "auto";

    updatePackSiblingsOptionUi(type, baseGroups, currentMode);
    const groups = getEffectiveActionGroups(state.pendingAction);
    state.pendingAction.groups = groups;

    const payload = buildActionPayloadForGroups(type, groups);
    const ro = state.config.read_only_mode;

    $("actionReadOnlyBanner").hidden = !ro;
    $("unlockAndExecuteBtn").hidden = !ro;
    $("confirmActionBtn").disabled = ro;
    $("actionPreviewSummary").textContent = "Running dry-run validation...";
    $("actionPreviewList").innerHTML = "";

    let inspectResult = null;
    if (
      (type === "arr-import" || type === "arr-fix") &&
      payload.importItems &&
      payload.importItems.length > 0 &&
      payload.import_mode === "move"
    ) {
      const inspectReq = collectInspectInput(groups, payload.importItems);
      if (inspectReq.torrents.length > 0) {
        try {
          inspectResult = await apiFetch("/api/actions/qbit-relocate/inspect", {
            method: "POST",
            body: JSON.stringify(inspectReq),
          });
          if (previewEpoch !== state.actionPreviewEpoch) return;
          state.lastInspectResult = inspectResult;
        } catch (err) {
          if (previewEpoch !== state.actionPreviewEpoch) return;
          state.lastInspectResult = null;
          inspectResult = {
            torrents: [],
            errors: [{ instance_id: "", torrent_hash: "", error: err.message }],
          };
        }
      } else {
        state.lastInspectResult = null;
      }
    } else {
      state.lastInspectResult = null;
    }

    updateQbitRelocateOptionUi(type, groups, payload, inspectResult);
    renderActionExplanation(type, groups, payload, options || {}, inspectResult);

    try {
      let result;
      if (type === "convert-hardlinks") {
        result = await apiFetch("/api/actions/convert-hardlinks", {
          method: "POST",
          body: JSON.stringify({ pairs: payload.pairs, dry_run: true }),
        });
        if (previewEpoch !== state.actionPreviewEpoch) return;
        $("actionPreviewSummary").innerHTML =
          `<strong>${result.converted_count}</strong> copy pair(s) verified identical (SHA-256 head/mid/tail) and ready to hardlink. Disk space reclaimed: <strong>${formatBytes(result.bytes_reclaimed)}</strong>.`;
        $("actionPreviewList").innerHTML = [
          ...result.converted.map(
            (c) => `<div class="preview-row">LINK: ${escapeHtml(c.source_path)} &rarr; ${escapeHtml(c.target_path)} (${formatBytes(c.bytes_reclaimed)})</div>`
          ),
          ...result.errors.map(
            (e) => `<div class="preview-row preview-err">SKIP: ${escapeHtml(e.target_path)} — ${escapeHtml(e.error)}</div>`
          ),
        ].join("") || `<div class="preview-row">No eligible pairs found.</div>`;
        if (result.converted_count === 0) {
          $("confirmActionBtn").disabled = true;
          $("unlockAndExecuteBtn").hidden = true;
        }
      } else if (type === "delete-torrents") {
        result = await apiFetch("/api/actions/delete-torrents", {
          method: "POST",
          body: JSON.stringify({ items: payload.items, delete_files: payload.delete_files, dry_run: true }),
        });
        if (previewEpoch !== state.actionPreviewEpoch) return;
        $("actionPreviewSummary").innerHTML =
          `<strong>${result.deleted_count}</strong> torrent(s) queued for removal${payload.delete_files ? " <strong>with data files</strong>" : " (keeping files on disk)"}. Estimated disk space freed: <strong>${formatBytes(result.bytes_reclaimed)}</strong>.`;
        $("actionPreviewList").innerHTML = [
          ...payload.items.map(
            (it) => `<div class="preview-row">[${escapeHtml(it.instance_name)}] ${escapeHtml(it.torrent_name)}<br><small>${escapeHtml((it.expected_paths || []).join(", "))}</small></div>`
          ),
          ...result.errors.map((e) => `<div class="preview-row preview-err">${escapeHtml(e.error)}</div>`),
        ].join("") || `<div class="preview-row">No matching torrents found.</div>`;
        if (result.deleted_count === 0) {
          $("confirmActionBtn").disabled = true;
          $("unlockAndExecuteBtn").hidden = true;
        }
      } else if (type === "delete-files") {
        result = await apiFetch("/api/actions/delete-files", {
          method: "POST",
          body: JSON.stringify({ file_paths: payload.file_paths, cleanup_empty_dirs: true, dry_run: true }),
        });
        if (previewEpoch !== state.actionPreviewEpoch) return;
        $("actionPreviewSummary").innerHTML =
          `<strong>${result.deleted_count}</strong> file(s) will be deleted from disk. Estimated physical space freed: <strong>${formatBytes(result.bytes_reclaimed)}</strong>.`;
        $("actionPreviewList").innerHTML = [
          ...result.deleted_paths.map((p) => `<div class="preview-row">DELETE: ${escapeHtml(p)}</div>`),
          ...result.errors.map((e) => `<div class="preview-row preview-err">SKIP: ${escapeHtml(e.path)} — ${escapeHtml(e.error)}</div>`),
        ].join("") || `<div class="preview-row">No eligible files found.</div>`;
        if (result.deleted_count === 0) {
          $("confirmActionBtn").disabled = true;
          $("unlockAndExecuteBtn").hidden = true;
        }
      } else if (type === "arr-import" || type === "arr-rescan" || type === "arr-fix") {
        let totalTriggered = 0;
        const summaryParts = [];
        const rowsHtml = [];

        if (payload.importItems.length > 0) {
          const impRes = await apiFetch("/api/actions/arr-import", {
            method: "POST",
            body: JSON.stringify({
              items: payload.importItems,
              force_manual_import: payload.force_manual_import,
              import_mode: payload.import_mode,
              dry_run: true,
            }),
          });
          if (previewEpoch !== state.actionPreviewEpoch) return;
          totalTriggered += impRes.triggered_count;
          summaryParts.push(`<strong>${impRes.triggered_count}</strong> import scan(s)`);
          for (const r of impRes.results) {
            const rejHtml = (r.rejections || []).length > 0
              ? `<br><small style="color:#fca5a5;">Rejections: ${escapeHtml(r.rejections.join("; "))}</small>`
              : "";
            const parsedHtml = r.parsed_title
              ? `<br><small>Arr parsed: ${escapeHtml(r.parsed_title)}</small>`
              : "";
            const modeLabel = (r.import_mode || payload.import_mode || "auto").toUpperCase();
            const destHtml = r.target_root_path
              ? `<span class="preview-dest">&rarr; Target library folder: ${escapeHtml(r.target_root_path)}</span>`
              : "";
            rowsHtml.push(
              `<div class="preview-row">[${escapeHtml(r.instance_name)}] ${escapeHtml(r.command_name)} (mode: ${escapeHtml(modeLabel)}): ${escapeHtml(r.remote_path)}${destHtml}${parsedHtml}${rejHtml}</div>`
            );
          }
          for (const e of impRes.errors) {
            rowsHtml.push(`<div class="preview-row preview-err">SKIP: ${escapeHtml(e.target)} — ${escapeHtml(e.error)}</div>`);
          }
        }

        if (payload.rescanItems.length > 0) {
          const resRes = await apiFetch("/api/actions/arr-rescan", {
            method: "POST",
            body: JSON.stringify({ items: payload.rescanItems, dry_run: true }),
          });
          if (previewEpoch !== state.actionPreviewEpoch) return;
          totalTriggered += resRes.triggered_count;
          summaryParts.push(`<strong>${resRes.triggered_count}</strong> library rescan(s)`);
          for (const r of resRes.results) {
            rowsHtml.push(
              `<div class="preview-row">[${escapeHtml(r.instance_name)}] ${escapeHtml(r.command_name)}: ${escapeHtml(r.title)} (ID #${escapeHtml(r.item_id)})</div>`
            );
          }
          for (const e of resRes.errors) {
            rowsHtml.push(`<div class="preview-row preview-err">SKIP: ${escapeHtml(e.target)} — ${escapeHtml(e.error)}</div>`);
          }
        }

        const relocateRow = $("qbitRelocateOptionRow");
        const relocateCheck = $("relocateQbitAfterMoveCheckbox");
        const moveCompanions = $("moveCompanionFilesCheckbox") ? $("moveCompanionFilesCheckbox").checked : true;
        const moveSamples = $("moveSampleFilesCheckbox") ? $("moveSampleFilesCheckbox").checked : false;
        if (
          payload.import_mode === "move" &&
          relocateRow &&
          !relocateRow.hidden &&
          relocateCheck &&
          relocateCheck.checked
        ) {
          const relocateInfo = buildQbitRelocatePreview(groups, inspectResult);
          for (const t of relocateInfo.eligible) {
            const compFiles = (t.files || []).filter((f) => f.kind === "metadata" || f.kind === "subtitle");
            const sampleFiles = (t.files || []).filter((f) => f.kind === "sample");
            const detailParts = [`${t.selected_count || t.file_count} of ${t.file_count} file(s)`];
            if (compFiles.length > 0) {
              detailParts.push(
                moveCompanions
                  ? `+ ${compFiles.length} companion(s) (.nfo/.srt)`
                  : `WARNING: ${compFiles.length} companion(s) NOT moved`
              );
            }
            if (sampleFiles.length > 0) {
              detailParts.push(
                moveSamples
                  ? `+ ${sampleFiles.length} sample(s) included`
                  : `${sampleFiles.length} sample(s) excluded`
              );
            }
            const warnCls = (compFiles.length > 0 && !moveCompanions) || sampleFiles.length > 0 ? "preview-warn" : "";
            rowsHtml.push(
              `<div class="preview-row ${warnCls}">QBIT RELOCATE: [${escapeHtml(t.instance_name)}] ${escapeHtml(t.torrent_name)} (${escapeHtml(detailParts.join(" • "))}) &mdash; will update save path/names &amp; recheck after Arr finishes</div>`
            );
          }
          for (const t of relocateInfo.blocked) {
            const msg = (t.warnings && t.warnings[0]) || "Cannot auto-relocate";
            rowsHtml.push(
              `<div class="preview-row preview-err">QBIT SKIP (${escapeHtml(t.status || "blocked")}): [${escapeHtml(t.instance_name)}] ${escapeHtml(t.torrent_name)} &mdash; ${escapeHtml(msg)}</div>`
            );
          }
          for (const e of relocateInfo.errors) {
            rowsHtml.push(
              `<div class="preview-row preview-err">QBIT INSPECT ERROR: ${escapeHtml(e.torrent_hash || "")} &mdash; ${escapeHtml(e.error)}</div>`
            );
          }
        }

        $("actionPreviewSummary").innerHTML = summaryParts.length > 0
          ? `Will send ${summaryParts.join(" and ")} to Radarr/Sonarr.`
          : "No eligible Radarr/Sonarr actions found.";
        $("actionPreviewList").innerHTML = rowsHtml.join("") || `<div class="preview-row">No eligible items found.</div>`;
        if (totalTriggered === 0) {
          $("confirmActionBtn").disabled = true;
          $("unlockAndExecuteBtn").hidden = true;
        }
      }
    } catch (err) {
      if (previewEpoch !== state.actionPreviewEpoch) return;
      $("actionPreviewSummary").textContent = `Preview failed: ${err.message}`;
      $("confirmActionBtn").disabled = true;
    }
  }

  async function executePendingAction(unlockFirst = false) {
    if (!state.pendingAction) return;
    const { type } = state.pendingAction;
    const groups = getEffectiveActionGroups(state.pendingAction);

    try {
      if (unlockFirst && state.config.read_only_mode) {
        state.config = await apiFetch("/api/config/read-only", {
          method: "PATCH",
          body: JSON.stringify({ read_only_mode: false }),
        });
        renderHeaderReadOnly();
      }

      const payload = buildActionPayloadForGroups(type, groups);
      const affectedIds = expandAffectedGroupIds(type, groups, payload);
      const waitDeletedPaths = collectWaitDeletedPaths(type, groups, payload);
      const triggeredCommands = [];
      let relocatePlan = null;

      if (
        (type === "arr-import" || type === "arr-fix") &&
        payload.import_mode === "move" &&
        $("qbitRelocateOptionRow") &&
        !$("qbitRelocateOptionRow").hidden &&
        $("relocateQbitAfterMoveCheckbox") &&
        $("relocateQbitAfterMoveCheckbox").checked &&
        !$("relocateQbitAfterMoveCheckbox").disabled &&
        state.lastInspectResult
      ) {
        const relocatableTorrents = (state.lastInspectResult.torrents || []).filter((t) => t.can_relocate);
        if (relocatableTorrents.length > 0) {
          const groupPreMoveMap = new Map();
          for (const g of groups || []) {
            const oldPaths = new Set((g.all_paths || []).map((p) => String(p).toLowerCase()));
            for (const m of g.arr_matches || []) {
              if (m.file_path) oldPaths.add(String(m.file_path).toLowerCase());
            }
            const inodeKeys = new Set();
            for (const f of [...(g.folder_a_files || []), ...(g.folder_b_files || [])]) {
              const ino = f.inode_str || (f.inode ? String(f.inode) : "");
              if (ino && ino !== "0") {
                inodeKeys.add(`${f.device || 0}:${ino}`);
              }
            }
            groupPreMoveMap.set(g.id, { oldPaths, inodeKeys });
          }

          relocatePlan = {
            moveCompanions: $("moveCompanionFilesCheckbox") ? $("moveCompanionFilesCheckbox").checked : true,
            moveSamples: $("moveSampleFilesCheckbox") ? $("moveSampleFilesCheckbox").checked : false,
            inspectedTorrents: relocatableTorrents,
            groupPreMoveMap,
          };

          const relHashSet = new Set(relocatableTorrents.map((t) => `${t.instance_id}::${String(t.torrent_hash).toLowerCase()}`));
          if (state.scan && Array.isArray(state.scan.groups)) {
            for (const g of state.scan.groups) {
              if ((g.torrents || []).some((t) => relHashSet.has(`${t.instance_id}::${String(t.torrent_hash).toLowerCase()}`))) {
                if (!affectedIds.includes(g.id)) affectedIds.push(g.id);
              }
            }
          }
        }
      }

      let res;

      if (type === "convert-hardlinks") {
        res = await apiFetch("/api/actions/convert-hardlinks", {
          method: "POST",
          body: JSON.stringify({ pairs: payload.pairs, dry_run: false }),
        });
        showToast(
          `Converted ${res.converted_count} file(s) to hardlinks (${formatBytes(res.bytes_reclaimed)} reclaimed).`,
          "success"
        );
      } else if (type === "delete-torrents") {
        res = await apiFetch("/api/actions/delete-torrents", {
          method: "POST",
          body: JSON.stringify({ items: payload.items, delete_files: payload.delete_files, dry_run: false }),
        });
        showToast(
          `Deleted ${res.deleted_count} torrent(s) (${formatBytes(res.bytes_reclaimed)} reclaimed).`,
          "success"
        );
      } else if (type === "delete-files") {
        res = await apiFetch("/api/actions/delete-files", {
          method: "POST",
          body: JSON.stringify({ file_paths: payload.file_paths, cleanup_empty_dirs: true, dry_run: false }),
        });
        showToast(
          `Deleted ${res.deleted_count} file(s) (${formatBytes(res.bytes_reclaimed)} freed).`,
          "success"
        );
      } else if (type === "arr-import" || type === "arr-rescan" || type === "arr-fix") {
        let totalTriggered = 0;
        let errCount = 0;
        if (payload.importItems.length > 0) {
          const impRes = await apiFetch("/api/actions/arr-import", {
            method: "POST",
            body: JSON.stringify({
              items: payload.importItems,
              force_manual_import: payload.force_manual_import,
              import_mode: payload.import_mode,
              dry_run: false,
            }),
          });
          totalTriggered += impRes.triggered_count;
          errCount += (impRes.errors || []).length;
          for (const r of impRes.results || []) {
            if (r.instance_id && r.command_id > 0) {
              triggeredCommands.push({ instance_id: r.instance_id, command_id: r.command_id });
            }
          }
        }
        if (payload.rescanItems.length > 0) {
          const resRes = await apiFetch("/api/actions/arr-rescan", {
            method: "POST",
            body: JSON.stringify({ items: payload.rescanItems, dry_run: false }),
          });
          totalTriggered += resRes.triggered_count;
          errCount += (resRes.errors || []).length;
          for (const r of resRes.results || []) {
            if (r.instance_id && r.command_id > 0) {
              triggeredCommands.push({ instance_id: r.instance_id, command_id: r.command_id });
            }
          }
        }
        showToast(
          `Triggered ${totalTriggered} Radarr/Sonarr command(s)${errCount > 0 ? ` (${errCount} error(s))` : ""}.`,
          errCount > 0 && totalTriggered === 0 ? "error" : "success"
        );
      }

      $("actionModal").hidden = true;
      state.pendingAction = null;

      if (type === "arr-import" || type === "arr-rescan" || type === "arr-fix") {
        for (const id of affectedIds) {
          state.pendingArrRefreshGroups.add(id);
        }
        renderTable();
        void pollArrCommandsAndRefresh(triggeredCommands, affectedIds, relocatePlan);
      } else {
        const refRes = await refreshGroupsInPlace(affectedIds, waitDeletedPaths, false);
        if (refRes && !refRes.full_scan) {
          const survivingArrIds = (refRes.updated_groups || [])
            .filter((g) => g.arr_summary !== "UNCONFIGURED" && (g.arr_matches || []).length > 0)
            .map((g) => g.id);
          if (survivingArrIds.length > 0) {
            void syncSurvivingGroupsWithArr(survivingArrIds);
          }
        }
      }
    } catch (err) {
      showToast(`Action failed: ${err.message}`, "error");
    }
  }

  // --- Settings Modal & Visual Folder Picker ---
  function switchSettingsTab(tabName) {
    state.activeSettingsTab = tabName || "folders";
    document.querySelectorAll("#settingsTabBar .settings-tab-btn").forEach((btn) => {
      btn.classList.toggle("active", btn.dataset.settingsTab === state.activeSettingsTab);
    });
    document.querySelectorAll("[data-settings-panel]").forEach((panel) => {
      panel.hidden = panel.dataset.settingsPanel !== state.activeSettingsTab;
    });
  }

  function openSettingsModal() {
    state.draftConfig = JSON.parse(JSON.stringify(state.config));
    state.draftConfig.folders = state.draftConfig.folders || [];
    state.draftConfig.qbit_instances = state.draftConfig.qbit_instances || [];
    state.draftConfig.arr_instances = state.draftConfig.arr_instances || [];
    if (state.draftConfig.folders.length === 0) {
      state.draftConfig.folders.push(
        { id: randomId("fld"), label: "Folder A (Original / Primary)", path: "/data/torrents", role: "primary" },
        { id: randomId("fld"), label: "Folder B (Cross-Seed)", path: "/data/cross-seed", role: "secondary" }
      );
    }
    $("ignoreExtInput").value = (state.draftConfig.ignore_extensions || []).join(", ");
    $("minSizeInput").value = String(state.draftConfig.min_size_bytes || 0);
    renderSettingsEditors();
    switchSettingsTab(state.activeSettingsTab || "folders");
    $("settingsModal").hidden = false;
  }

  function renderSettingsEditors() {
    if ($("settingsCountFolders")) {
      $("settingsCountFolders").textContent = String((state.draftConfig.folders || []).length);
    }
    if ($("settingsCountQbit")) {
      $("settingsCountQbit").textContent = String((state.draftConfig.qbit_instances || []).length);
    }
    if ($("settingsCountArr")) {
      $("settingsCountArr").textContent = String((state.draftConfig.arr_instances || []).length);
    }
    const foldersList = $("foldersEditorList");
    foldersList.innerHTML = state.draftConfig.folders
      .map(
        (f, idx) => `
      <div class="editor-card" data-folder-idx="${idx}">
        <div class="editor-grid">
          <label class="field-group">
            <span>Label</span>
            <input class="input-control folder-label-in" data-idx="${idx}" value="${escapeHtml(f.label)}" placeholder="e.g. Main Downloads" />
          </label>
          <label class="field-group">
            <span>Role</span>
            <select class="input-control folder-role-in" data-idx="${idx}">
              <option value="primary" ${f.role === "primary" ? "selected" : ""}>Primary (Folder A / Original)</option>
              <option value="secondary" ${f.role !== "primary" ? "selected" : ""}>Secondary (Folder B+ / Cross-Seed)</option>
            </select>
          </label>
        </div>
        <div class="field-group">
          <span>Directory Path</span>
          <div class="path-input-row">
            <input class="input-control folder-path-in" data-idx="${idx}" value="${escapeHtml(f.path)}" placeholder="/data/..." />
            <button class="btn btn-secondary btn-sm browse-folder-btn" data-idx="${idx}" type="button">Browse...</button>
            <button class="btn btn-outline-danger btn-sm remove-folder-btn" data-idx="${idx}" type="button">Remove</button>
          </div>
        </div>
      </div>`
      )
      .join("");

    const qbitList = $("qbitEditorList");
    if (state.draftConfig.qbit_instances.length === 0) {
      qbitList.innerHTML = `<div class="section-hint">No qBittorrent instances configured yet. Click "+ Add qBit Instance" above.</div>`;
    } else {
      qbitList.innerHTML = state.draftConfig.qbit_instances
        .map(
          (inst, idx) => `
        <div class="editor-card" data-qbit-idx="${idx}">
          <div class="editor-grid">
            <label class="field-group">
              <span>Instance Name</span>
              <input class="input-control qbit-name-in" data-idx="${idx}" value="${escapeHtml(inst.name)}" placeholder="qBit Main" />
            </label>
            <label class="field-group">
              <span>WebUI URL</span>
              <input class="input-control qbit-url-in" data-idx="${idx}" value="${escapeHtml(inst.url)}" placeholder="http://192.168.1.100:8080" />
            </label>
            <label class="field-group">
              <span>Username</span>
              <input class="input-control qbit-user-in" data-idx="${idx}" value="${escapeHtml(inst.username)}" placeholder="admin" />
            </label>
            <label class="field-group">
              <span>Password</span>
              <input class="input-control qbit-pass-in" data-idx="${idx}" type="password" value="${escapeHtml(inst.password)}" placeholder="••••••" />
            </label>
          </div>

          <div class="mapping-block">
            <div class="editor-card-footer">
              <span class="section-hint">Path Mappings (qBittorrent path &rarr; SeedLink container path)</span>
              <button class="btn btn-ghost btn-xs add-mapping-btn" data-idx="${idx}" type="button">+ Add Mapping</button>
            </div>
            ${(inst.path_mappings || [])
              .map(
                (m, mIdx) => `
              <div class="mapping-row">
                <input class="input-control map-remote-in" data-idx="${idx}" data-midx="${mIdx}" value="${escapeHtml(m.remote_prefix)}" placeholder="qBit prefix (e.g. /downloads)" />
                <input class="input-control map-local-in" data-idx="${idx}" data-midx="${mIdx}" value="${escapeHtml(m.local_prefix)}" placeholder="Local prefix (e.g. /data/torrents)" />
                <button class="btn btn-outline-danger btn-xs remove-map-btn" data-idx="${idx}" data-midx="${mIdx}" type="button">&times;</button>
              </div>`
              )
              .join("")}
          </div>

          <div class="editor-card-footer">
            <label style="display:flex;align-items:center;gap:6px;font-size:0.8rem;">
              <input type="checkbox" class="qbit-enabled-in" data-idx="${idx}" ${inst.enabled ? "checked" : ""} />
              Enabled
            </label>
            <div style="display:flex;align-items:center;gap:8px;">
              <span class="test-status" id="qbitTestStatus-${idx}"></span>
              <button class="btn btn-secondary btn-xs test-qbit-btn" data-idx="${idx}" type="button">Test Connection</button>
              <button class="btn btn-outline-danger btn-xs remove-qbit-btn" data-idx="${idx}" type="button">Remove</button>
            </div>
          </div>
        </div>`
        )
        .join("");
    }

    const arrList = $("arrEditorList");
    const arrInstances = state.draftConfig.arr_instances || [];
    if (arrInstances.length === 0) {
      arrList.innerHTML = `<div class="section-hint">No Radarr or Sonarr instances configured yet. Click "+ Add Radarr/Sonarr" above.</div>`;
    } else {
      arrList.innerHTML = arrInstances
        .map(
          (inst, idx) => `
        <div class="editor-card" data-arr-idx="${idx}">
          <div class="editor-grid">
            <label class="field-group">
              <span>Instance Name</span>
              <input class="input-control arr-name-in" data-idx="${idx}" value="${escapeHtml(inst.name)}" placeholder="Radarr" />
            </label>
            <label class="field-group">
              <span>Type</span>
              <select class="input-control arr-type-in" data-idx="${idx}">
                <option value="radarr" ${inst.type === "radarr" ? "selected" : ""}>Radarr</option>
                <option value="sonarr" ${inst.type === "sonarr" ? "selected" : ""}>Sonarr</option>
              </select>
            </label>
            <label class="field-group">
              <span>URL</span>
              <input class="input-control arr-url-in" data-idx="${idx}" value="${escapeHtml(inst.url)}" placeholder="http://192.168.1.100:7878" />
            </label>
            <label class="field-group">
              <span>API Key</span>
              <input class="input-control arr-key-in" data-idx="${idx}" type="password" value="${escapeHtml(inst.api_key)}" placeholder="API Key" />
            </label>
          </div>

          <div class="mapping-block">
            <div class="editor-card-footer">
              <span class="section-hint">Path Mappings (Radarr/Sonarr path &rarr; SeedLink container path)</span>
              <button class="btn btn-ghost btn-xs add-arr-mapping-btn" data-idx="${idx}" type="button">+ Add Mapping</button>
            </div>
            ${(inst.path_mappings || [])
              .map(
                (m, mIdx) => `
              <div class="mapping-row">
                <input class="input-control arr-map-remote-in" data-idx="${idx}" data-midx="${mIdx}" value="${escapeHtml(m.remote_prefix)}" placeholder="Arr prefix (e.g. /movies)" />
                <input class="input-control arr-map-local-in" data-idx="${idx}" data-midx="${mIdx}" value="${escapeHtml(m.local_prefix)}" placeholder="Local prefix (e.g. /data/media/movies)" />
                <button class="btn btn-outline-danger btn-xs remove-arr-map-btn" data-idx="${idx}" data-midx="${mIdx}" type="button">&times;</button>
              </div>`
              )
              .join("")}
          </div>

          <div class="editor-card-footer">
            <label style="display:flex;align-items:center;gap:6px;font-size:0.8rem;">
              <input type="checkbox" class="arr-enabled-in" data-idx="${idx}" ${inst.enabled ? "checked" : ""} />
              Enabled
            </label>
            <div style="display:flex;align-items:center;gap:8px;">
              <span class="test-status" id="arrTestStatus-${idx}"></span>
              <button class="btn btn-secondary btn-xs test-arr-btn" data-idx="${idx}" type="button">Test Connection</button>
              <button class="btn btn-outline-danger btn-xs remove-arr-btn" data-idx="${idx}" type="button">Remove</button>
            </div>
          </div>
        </div>`
        )
        .join("");
    }
  }

  function syncDraftFromDom() {
    if (!state.draftConfig) return;
    document.querySelectorAll(".folder-label-in").forEach((el) => {
      state.draftConfig.folders[Number(el.dataset.idx)].label = el.value.trim();
    });
    document.querySelectorAll(".folder-role-in").forEach((el) => {
      state.draftConfig.folders[Number(el.dataset.idx)].role = el.value;
    });
    document.querySelectorAll(".folder-path-in").forEach((el) => {
      state.draftConfig.folders[Number(el.dataset.idx)].path = el.value.trim();
    });

    document.querySelectorAll(".qbit-name-in").forEach((el) => {
      state.draftConfig.qbit_instances[Number(el.dataset.idx)].name = el.value.trim();
    });
    document.querySelectorAll(".qbit-url-in").forEach((el) => {
      state.draftConfig.qbit_instances[Number(el.dataset.idx)].url = el.value.trim();
    });
    document.querySelectorAll(".qbit-user-in").forEach((el) => {
      state.draftConfig.qbit_instances[Number(el.dataset.idx)].username = el.value;
    });
    document.querySelectorAll(".qbit-pass-in").forEach((el) => {
      state.draftConfig.qbit_instances[Number(el.dataset.idx)].password = el.value;
    });
    document.querySelectorAll(".qbit-enabled-in").forEach((el) => {
      state.draftConfig.qbit_instances[Number(el.dataset.idx)].enabled = el.checked;
    });
    document.querySelectorAll(".map-remote-in").forEach((el) => {
      state.draftConfig.qbit_instances[Number(el.dataset.idx)].path_mappings[Number(el.dataset.midx)].remote_prefix =
        el.value.trim();
    });
    document.querySelectorAll(".map-local-in").forEach((el) => {
      state.draftConfig.qbit_instances[Number(el.dataset.idx)].path_mappings[Number(el.dataset.midx)].local_prefix =
        el.value.trim();
    });

    document.querySelectorAll(".arr-name-in").forEach((el) => {
      state.draftConfig.arr_instances[Number(el.dataset.idx)].name = el.value.trim();
    });
    document.querySelectorAll(".arr-type-in").forEach((el) => {
      state.draftConfig.arr_instances[Number(el.dataset.idx)].type = el.value;
    });
    document.querySelectorAll(".arr-url-in").forEach((el) => {
      state.draftConfig.arr_instances[Number(el.dataset.idx)].url = el.value.trim();
    });
    document.querySelectorAll(".arr-key-in").forEach((el) => {
      state.draftConfig.arr_instances[Number(el.dataset.idx)].api_key = el.value.trim();
    });
    document.querySelectorAll(".arr-enabled-in").forEach((el) => {
      state.draftConfig.arr_instances[Number(el.dataset.idx)].enabled = el.checked;
    });
    document.querySelectorAll(".arr-map-remote-in").forEach((el) => {
      state.draftConfig.arr_instances[Number(el.dataset.idx)].path_mappings[Number(el.dataset.midx)].remote_prefix =
        el.value.trim();
    });
    document.querySelectorAll(".arr-map-local-in").forEach((el) => {
      state.draftConfig.arr_instances[Number(el.dataset.idx)].path_mappings[Number(el.dataset.midx)].local_prefix =
        el.value.trim();
    });

    const extsRaw = $("ignoreExtInput").value;
    state.draftConfig.ignore_extensions = extsRaw
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean);
    state.draftConfig.min_size_bytes = Math.max(0, Number($("minSizeInput").value || 0));
  }

  async function saveSettingsAndScan() {
    syncDraftFromDom();
    const cleaned = {
      ...state.draftConfig,
      folders: state.draftConfig.folders.filter((f) => f.label && f.path),
      qbit_instances: state.draftConfig.qbit_instances
        .filter((q) => q.name && q.url)
        .map((q) => ({
          ...q,
          path_mappings: (q.path_mappings || []).filter((m) => m.remote_prefix && m.local_prefix),
        })),
      arr_instances: (state.draftConfig.arr_instances || [])
        .filter((a) => a.name && a.url)
        .map((a) => ({
          ...a,
          path_mappings: (a.path_mappings || []).filter((m) => m.remote_prefix && m.local_prefix),
        })),
    };

    try {
      state.config = await apiFetch("/api/config", {
        method: "PUT",
        body: JSON.stringify(cleaned),
      });
      $("settingsModal").hidden = true;
      showToast("Settings saved.", "success");
      await runScan();
    } catch (err) {
      showToast(`Invalid settings: ${err.message}`, "error");
    }
  }

  // --- Visual Folder Picker ---
  async function openFolderPicker(folderIdx, initialPath) {
    state.pickerTargetIndex = folderIdx;
    $("folderPickerModal").hidden = false;
    await browsePickerPath(initialPath || "");
  }

  async function browsePickerPath(targetPath) {
    const listEl = $("pickerList");
    listEl.innerHTML = `<div class="picker-item">Loading...</div>`;
    try {
      const query = targetPath ? `?path=${encodeURIComponent(targetPath)}` : "";
      const data = await apiFetch(`/api/fs/browse${query}`);
      state.pickerCurrentPath = data.current_path;
      state.pickerParentPath = data.parent_path;
      $("pickerCurrentPath").value = data.current_path || "(Drives / Root)";
      $("pickerUpBtn").disabled = data.parent_path === null;

      if (data.directories.length === 0) {
        listEl.innerHTML = `<div class="picker-item" style="color:var(--text-muted)">No subdirectories</div>`;
        return;
      }

      listEl.innerHTML = data.directories
        .map(
          (d) => `
          <button class="picker-item" data-path="${escapeHtml(d.path)}" type="button">
            <span>&#128193;</span>
            <span>${escapeHtml(d.name)}</span>
          </button>`
        )
        .join("");
    } catch (err) {
      if (targetPath) {
        await browsePickerPath("");
      } else {
        listEl.innerHTML = `<div class="picker-item" style="color:#fca5a5">${escapeHtml(err.message)}</div>`;
      }
    }
  }

  function getCurrentPageSlice() {
    const filteredGroups = getFilteredGroups();
    const displayRows = buildPackDisplayRows(filteredGroups);
    const totalPages = Math.max(1, Math.ceil(displayRows.length / state.pageSize));
    const page = Math.min(Math.max(1, state.page), totalPages);
    const startIdx = (page - 1) * state.pageSize;
    const pageItems = displayRows.slice(startIdx, startIdx + state.pageSize);
    return { filteredGroups, displayRows, pageItems, totalPages, page };
  }

  function syncViewModeUi() {
    if ($("viewModeGroupedBtn")) {
      $("viewModeGroupedBtn").classList.toggle("active", state.viewMode === "grouped");
    }
    if ($("viewModeFlatBtn")) {
      $("viewModeFlatBtn").classList.toggle("active", state.viewMode !== "grouped");
    }
  }

  // --- Event Binding ---
  function bindEvents() {
    $("readOnlyToggleBtn").addEventListener("click", toggleReadOnly);
    $("scanNowBtn").addEventListener("click", runScan);
    $("openSettingsBtn").addEventListener("click", openSettingsModal);
    $("welcomeSettingsBtn").addEventListener("click", openSettingsModal);

    $("exportCsvBtn").addEventListener("click", () => {
      window.location.href = "/api/export?format=csv";
    });
    $("exportJsonBtn").addEventListener("click", () => {
      window.location.href = "/api/export?format=json";
    });

    document.querySelectorAll("#quickTabs .tab-btn").forEach((btn) => {
      btn.addEventListener("click", () => {
        state.activeTab = btn.dataset.tab;
        state.page = 1;
        syncActiveTabUi();
        renderTable();
      });
    });

    document.querySelectorAll("#kpiGrid .kpi-card").forEach((card) => {
      card.addEventListener("click", () => {
        state.activeTab = card.dataset.filterTab;
        state.page = 1;
        syncActiveTabUi();
        renderTable();
      });
    });

    $("searchInput").addEventListener("input", (e) => {
      state.search = e.target.value;
      state.page = 1;
      renderTable();
    });
    $("seedingFilter").addEventListener("change", (e) => {
      state.seedingFilter = e.target.value;
      state.page = 1;
      renderTable();
    });
    $("arrFilter").addEventListener("change", (e) => {
      state.arrFilter = e.target.value;
      state.page = 1;
      renderTable();
    });
    $("folderFilter").addEventListener("change", (e) => {
      state.folderFilter = e.target.value;
      state.page = 1;
      renderTable();
    });
    $("sortSelect").addEventListener("change", (e) => {
      state.sortBy = e.target.value;
      state.page = 1;
      renderTable();
    });
    $("pageSizeSelect").addEventListener("change", (e) => {
      state.pageSize = Math.max(10, Number(e.target.value) || 100);
      state.page = 1;
      renderTable();
    });
    $("pageFirstBtn").addEventListener("click", () => {
      state.page = 1;
      renderTable();
    });
    $("pagePrevBtn").addEventListener("click", () => {
      if (state.page > 1) {
        state.page -= 1;
        renderTable();
      }
    });
    $("pageNextBtn").addEventListener("click", () => {
      const { totalPages } = getCurrentPageSlice();
      if (state.page < totalPages) {
        state.page += 1;
        renderTable();
      }
    });
    $("pageLastBtn").addEventListener("click", () => {
      const { totalPages } = getCurrentPageSlice();
      state.page = totalPages;
      renderTable();
    });

    $("viewModeGroupedBtn").addEventListener("click", () => {
      if (state.viewMode !== "grouped") {
        state.viewMode = "grouped";
        state.page = 1;
        syncViewModeUi();
        renderTable();
      }
    });
    $("viewModeFlatBtn").addEventListener("click", () => {
      if (state.viewMode !== "flat") {
        state.viewMode = "flat";
        state.page = 1;
        syncViewModeUi();
        renderTable();
      }
    });

    $("expandPageBtn").addEventListener("click", () => {
      const { pageItems } = getCurrentPageSlice();
      if (pageItems.length === 0) return;
      const isExpanded = (it) =>
        it.kind === "pack" ? state.expandedPackIds.has(it.id) : state.expandedGroupIds.has(it.id);
      const allExpanded = pageItems.every(isExpanded);
      for (const it of pageItems) {
        if (it.kind === "pack") {
          if (allExpanded) state.expandedPackIds.delete(it.id);
          else state.expandedPackIds.add(it.id);
        } else {
          if (allExpanded) state.expandedGroupIds.delete(it.id);
          else state.expandedGroupIds.add(it.id);
        }
      }
      renderTable();
    });

    $("selectAllCheckbox").addEventListener("change", (e) => {
      const { pageItems } = getCurrentPageSlice();
      for (const it of pageItems) {
        for (const m of it.members || [it.group]) {
          if (!m || !m.id) continue;
          if (e.target.checked) state.selectedGroupIds.add(m.id);
          else state.selectedGroupIds.delete(m.id);
        }
      }
      renderTable();
    });
    $("selectAllMatchingBtn").addEventListener("click", () => {
      const filtered = getFilteredGroups();
      filtered.forEach((g) => state.selectedGroupIds.add(g.id));
      renderTable();
    });
    $("clearSelectionBtn").addEventListener("click", () => {
      state.selectedGroupIds.clear();
      renderTable();
    });

    $("auditTableBody").addEventListener("click", async (e) => {
      const target = e.target;
      if (target.classList.contains("pack-checkbox")) {
        const packId = target.dataset.packId;
        const pack = state.currentPackMap.get(packId);
        if (pack) {
          for (const m of pack.members) {
            if (target.checked) state.selectedGroupIds.add(m.id);
            else state.selectedGroupIds.delete(m.id);
          }
          renderTable();
        }
        return;
      }

      if (target.classList.contains("row-checkbox")) {
        const gid = target.dataset.groupId;
        if (target.checked) state.selectedGroupIds.add(gid);
        else state.selectedGroupIds.delete(gid);
        renderTable();
        return;
      }

      const btn = target.closest("button");
      if (btn) {
        if (!state.scan) return;
        if (btn.dataset.packId) {
          const packId = btn.dataset.packId;
          if (btn.classList.contains("pack-expand-btn")) {
            if (state.expandedPackIds.has(packId)) state.expandedPackIds.delete(packId);
            else state.expandedPackIds.add(packId);
            renderTable();
            return;
          }
          const pack = state.currentPackMap.get(packId);
          if (!pack) return;

          if (btn.classList.contains("pack-arr-import")) {
            openActionModal("arr-import", pack.members);
          } else if (btn.classList.contains("pack-arr-rescan")) {
            openActionModal("arr-rescan", pack.members);
          } else if (btn.classList.contains("pack-delete-errored-torrent")) {
            const erroredGroups = pack.members
              .map((m) => ({ ...m, torrents: (m.torrents || []).filter((t) => Boolean(t.tracker_health)) }))
              .filter((m) => m.torrents.length > 0);
            openActionModal("delete-torrents", erroredGroups, { erroredOnly: true });
          } else if (btn.classList.contains("pack-delete-torrent")) {
            openActionModal("delete-torrents", pack.members);
          } else if (btn.classList.contains("pack-hardlink")) {
            openActionModal("convert-hardlinks", pack.members);
          } else if (btn.classList.contains("pack-delete-file")) {
            openActionModal("delete-files", pack.members);
          } else if (btn.classList.contains("pack-refresh")) {
            btn.disabled = true;
            btn.textContent = "...";
            try {
              const ids = pack.members.map((m) => m.id);
              const refRes = await refreshGroupsInPlace(ids, [], true);
              if (refRes) {
                showToast(`Refreshed ${ids.length} pack file(s) in ${refRes.duration_ms} ms.`, "success");
              }
            } catch (err) {
              showToast(`Pack refresh failed: ${err.message}`, "error");
              if (btn.isConnected) {
                btn.disabled = false;
                btn.innerHTML = "&#8635;";
              }
            }
          }
          return;
        }

        const gid = btn.dataset.groupId;
        if (btn.classList.contains("row-expand-btn")) {
          if (state.expandedGroupIds.has(gid)) state.expandedGroupIds.delete(gid);
          else state.expandedGroupIds.add(gid);
          renderTable();
          return;
        }
        const grp = state.scan.groups.find((g) => g.id === gid);
        if (!grp) return;

        if (btn.classList.contains("row-arr-import")) {
          openActionModal("arr-import", [grp]);
        } else if (btn.classList.contains("row-arr-rescan")) {
          openActionModal("arr-rescan", [grp]);
        } else if (btn.classList.contains("row-delete-errored-torrent")) {
          openActionModal("delete-torrents", [
            { ...grp, torrents: grp.torrents.filter((t) => Boolean(t.tracker_health)) },
          ], { erroredOnly: true });
        } else if (btn.classList.contains("row-delete-torrent")) {
          openActionModal("delete-torrents", [grp]);
        } else if (btn.classList.contains("row-hardlink")) {
          openActionModal("convert-hardlinks", [grp]);
        } else if (btn.classList.contains("row-delete-file")) {
          openActionModal("delete-files", [grp]);
        } else if (btn.classList.contains("row-refresh")) {
          btn.disabled = true;
          btn.textContent = "...";
          try {
            const refRes = await refreshGroupsInPlace([grp.id], [], true);
            if (refRes) {
              if ((refRes.removed_group_ids || []).includes(grp.id)) {
                showToast("Item no longer on disk — removed from list.", "info");
              } else {
                showToast(`Refreshed item in ${refRes.duration_ms} ms.`, "success");
              }
            }
          } catch (err) {
            showToast(`Row refresh failed: ${err.message}`, "error");
            if (btn.isConnected) {
              btn.disabled = false;
              btn.innerHTML = "&#8635;";
            }
          }
        }
        return;
      }

      if (target.closest("input, a, select, label")) return;
      const packRow = target.closest("tr.pack-row");
      if (packRow && packRow.dataset.packId) {
        const packId = packRow.dataset.packId;
        if (state.expandedPackIds.has(packId)) state.expandedPackIds.delete(packId);
        else state.expandedPackIds.add(packId);
        renderTable();
        return;
      }
      const mainRow = target.closest("tr.main-row");
      if (!mainRow || mainRow.closest(".detail-row")) return;
      const gid = mainRow.dataset.groupId;
      if (!gid) return;
      if (state.expandedGroupIds.has(gid)) state.expandedGroupIds.delete(gid);
      else state.expandedGroupIds.add(gid);
      renderTable();
    });

    $("bulkArrFixBtn").addEventListener("click", () => {
      openActionModal("arr-fix", getSelectedGroups());
    });
    $("bulkDeleteTorrentsBtn").addEventListener("click", () => {
      openActionModal("delete-torrents", getSelectedGroups());
    });
    $("bulkHardlinkBtn").addEventListener("click", () => {
      openActionModal("convert-hardlinks", getSelectedGroups());
    });
    $("bulkDeleteFilesBtn").addEventListener("click", () => {
      openActionModal("delete-files", getSelectedGroups());
    });

    $("closeActionBtn").addEventListener("click", () => ($("actionModal").hidden = true));
    $("cancelActionBtn").addEventListener("click", () => ($("actionModal").hidden = true));
    $("deleteTorrentFilesCheckbox").addEventListener("change", refreshActionDryRun);
    $("arrImportModeSelect").addEventListener("change", refreshActionDryRun);
    $("includePackSiblingsCheckbox").addEventListener("change", refreshActionDryRun);
    $("relocateQbitAfterMoveCheckbox").addEventListener("change", refreshActionDryRun);
    $("moveCompanionFilesCheckbox").addEventListener("change", refreshActionDryRun);
    $("moveSampleFilesCheckbox").addEventListener("change", refreshActionDryRun);
    $("forceManualImportCheckbox").addEventListener("change", refreshActionDryRun);
    $("confirmActionBtn").addEventListener("click", () => executePendingAction(false));
    $("unlockAndExecuteBtn").addEventListener("click", () => executePendingAction(true));

    $("closeSettingsBtn").addEventListener("click", () => ($("settingsModal").hidden = true));
    $("cancelSettingsBtn").addEventListener("click", () => ($("settingsModal").hidden = true));
    $("saveSettingsBtn").addEventListener("click", saveSettingsAndScan);
    $("settingsTabBar").addEventListener("click", (e) => {
      const btn = e.target.closest(".settings-tab-btn");
      if (btn && btn.dataset.settingsTab) {
        switchSettingsTab(btn.dataset.settingsTab);
      }
    });

    $("addFolderBtn").addEventListener("click", () => {
      syncDraftFromDom();
      const idx = state.draftConfig.folders.length + 1;
      state.draftConfig.folders.push({
        id: randomId("fld"),
        label: idx === 1 ? "Folder A (Primary)" : `Folder ${String.fromCharCode(64 + idx)}`,
        path: "",
        role: idx === 1 ? "primary" : "secondary",
      });
      renderSettingsEditors();
    });

    $("addQbitBtn").addEventListener("click", () => {
      syncDraftFromDom();
      state.draftConfig.qbit_instances.push({
        id: randomId("qbit"),
        name: `qBit ${state.draftConfig.qbit_instances.length + 1}`,
        url: "http://192.168.1.100:8080",
        username: "admin",
        password: "",
        enabled: true,
        path_mappings: [],
      });
      renderSettingsEditors();
    });

    $("addArrBtn").addEventListener("click", () => {
      syncDraftFromDom();
      state.draftConfig.arr_instances = state.draftConfig.arr_instances || [];
      const isSecond = state.draftConfig.arr_instances.length % 2 === 1;
      state.draftConfig.arr_instances.push({
        id: randomId("arr"),
        name: isSecond ? "Sonarr" : "Radarr",
        type: isSecond ? "sonarr" : "radarr",
        url: isSecond ? "http://192.168.1.100:8989" : "http://192.168.1.100:7878",
        api_key: "",
        enabled: true,
        path_mappings: [],
      });
      renderSettingsEditors();
    });

    $("foldersEditorList").addEventListener("click", (e) => {
      const btn = e.target.closest("button");
      if (!btn) return;
      syncDraftFromDom();
      const idx = Number(btn.dataset.idx);
      if (btn.classList.contains("remove-folder-btn")) {
        state.draftConfig.folders.splice(idx, 1);
        renderSettingsEditors();
      } else if (btn.classList.contains("browse-folder-btn")) {
        openFolderPicker(idx, state.draftConfig.folders[idx].path);
      }
    });

    $("qbitEditorList").addEventListener("click", async (e) => {
      const btn = e.target.closest("button");
      if (!btn) return;
      syncDraftFromDom();
      const idx = Number(btn.dataset.idx);

      if (btn.classList.contains("remove-qbit-btn")) {
        state.draftConfig.qbit_instances.splice(idx, 1);
        renderSettingsEditors();
      } else if (btn.classList.contains("add-mapping-btn")) {
        state.draftConfig.qbit_instances[idx].path_mappings.push({ remote_prefix: "", local_prefix: "" });
        renderSettingsEditors();
      } else if (btn.classList.contains("remove-map-btn")) {
        const mIdx = Number(btn.dataset.midx);
        state.draftConfig.qbit_instances[idx].path_mappings.splice(mIdx, 1);
        renderSettingsEditors();
      } else if (btn.classList.contains("test-qbit-btn")) {
        const statusEl = $(`qbitTestStatus-${idx}`);
        statusEl.className = "test-status";
        statusEl.textContent = "Testing...";
        try {
          const res = await apiFetch("/api/qbit/test", {
            method: "POST",
            body: JSON.stringify(state.draftConfig.qbit_instances[idx]),
          });
          if (res.ok) {
            statusEl.className = "test-status test-ok";
            statusEl.textContent = `Connected (${res.version}, ${res.torrent_count} torrents)`;
          } else {
            statusEl.className = "test-status test-fail";
            statusEl.textContent = `Failed: ${res.error || "Unknown error"}`;
          }
        } catch (err) {
          statusEl.className = "test-status test-fail";
          statusEl.textContent = `Failed: ${err.message}`;
        }
      }
    });

    $("arrEditorList").addEventListener("click", async (e) => {
      const btn = e.target.closest("button");
      if (!btn) return;
      syncDraftFromDom();
      const idx = Number(btn.dataset.idx);

      if (btn.classList.contains("remove-arr-btn")) {
        state.draftConfig.arr_instances.splice(idx, 1);
        renderSettingsEditors();
      } else if (btn.classList.contains("add-arr-mapping-btn")) {
        state.draftConfig.arr_instances[idx].path_mappings.push({ remote_prefix: "", local_prefix: "" });
        renderSettingsEditors();
      } else if (btn.classList.contains("remove-arr-map-btn")) {
        const mIdx = Number(btn.dataset.midx);
        state.draftConfig.arr_instances[idx].path_mappings.splice(mIdx, 1);
        renderSettingsEditors();
      } else if (btn.classList.contains("test-arr-btn")) {
        const statusEl = $(`arrTestStatus-${idx}`);
        statusEl.className = "test-status";
        statusEl.textContent = "Testing...";
        try {
          const res = await apiFetch("/api/arr/test", {
            method: "POST",
            body: JSON.stringify(state.draftConfig.arr_instances[idx]),
          });
          if (res.ok) {
            statusEl.className = "test-status test-ok";
            statusEl.textContent = `Connected (${res.app_name || ""} v${res.version}, ${res.item_count} items)`;
          } else {
            statusEl.className = "test-status test-fail";
            statusEl.textContent = `Failed: ${res.error || "Unknown error"}`;
          }
        } catch (err) {
          statusEl.className = "test-status test-fail";
          statusEl.textContent = `Failed: ${err.message}`;
        }
      }
    });

    $("closePickerBtn").addEventListener("click", () => ($("folderPickerModal").hidden = true));
    $("cancelPickerBtn").addEventListener("click", () => ($("folderPickerModal").hidden = true));
    $("pickerUpBtn").addEventListener("click", () => {
      if (state.pickerParentPath !== null) {
        browsePickerPath(state.pickerParentPath);
      }
    });
    $("pickerList").addEventListener("click", (e) => {
      const btn = e.target.closest(".picker-item");
      if (btn && btn.dataset.path) {
        browsePickerPath(btn.dataset.path);
      }
    });
    $("selectPickerBtn").addEventListener("click", () => {
      if (state.pickerTargetIndex !== null && state.pickerCurrentPath) {
        state.draftConfig.folders[state.pickerTargetIndex].path = state.pickerCurrentPath;
        renderSettingsEditors();
      }
      $("folderPickerModal").hidden = true;
    });
  }

  async function init() {
    bindEvents();
    try {
      await loadConfig();
      if (state.config.folders.length > 0) {
        await runScan();
      }
    } catch (err) {
      showToast(`Initialization failed: ${err.message}`, "error");
    }
  }

  document.addEventListener("DOMContentLoaded", init);
})();
