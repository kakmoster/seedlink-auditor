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
    selectedGroupIds: new Set(),
    pendingArrRefreshGroups: new Set(),
    scanEpoch: 0,
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
            (m) => `${m.instance_name} ${m.app_type} ${m.title} ${m.status_message || ""}`
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
            <span class="inode-tag">#${escapeHtml(file.inode)}</span>
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

  function renderTable() {
    const tbody = $("auditTableBody");
    const filtered = getFilteredGroups();

    if (!state.scan) {
      tbody.innerHTML = `<tr><td colspan="7" class="empty-table-cell">No scan data yet. Open <strong>Settings (Folders &amp; qBit)</strong> to add your folders and click <strong>Scan Now</strong>.</td></tr>`;
      updateSelectionControls([]);
      return;
    }

    if (filtered.length === 0) {
      tbody.innerHTML = `<tr><td colspan="7" class="empty-table-cell">No files match the current filter criteria.</td></tr>`;
      updateSelectionControls([]);
      return;
    }

    tbody.innerHTML = filtered
      .map((grp) => {
        const checked = state.selectedGroupIds.has(grp.id) ? "checked" : "";
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
                  const msgHtml = t.tracker_msg
                    ? `<div class="tracker-error-msg">${escapeHtml(t.tracker_msg)}</div>`
                    : "";
                  return `
                <div class="torrent-item ${hasHealthIssue ? "has-error" : ""}">
                  <div class="torrent-top">
                    <span class="instance-badge">${escapeHtml(t.instance_name)}</span>
                    ${t.tracker_host ? `<span class="tracker-badge">${escapeHtml(t.tracker_host)}</span>` : ""}
                    <span class="nlink-badge">${escapeHtml(t.state)}</span>
                    ${healthBadge}
                  </div>
                  <div class="torrent-name">${escapeHtml(t.torrent_name)}</div>
                  ${msgHtml}
                  <div class="torrent-sub">Ratio: ${Number(t.ratio || 0).toFixed(2)} • Match: ${escapeHtml(t.match_type)}</div>
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
                    ${m.status_message ? `<div class="${isIssue ? "arr-msg" : "torrent-sub"}">${escapeHtml(m.status_message)}</div>` : ""}
                  </div>`;
                  })
                  .join("");

        const reclaimClass =
          grp.reclaimable_bytes > 0
            ? grp.status === "ORPHANED_IN_B"
              ? "reclaim-danger"
              : "reclaim-highlight"
            : "reclaim-zero";

        const actionBtns = [];
        if (arrMatches.some((m) => m.can_import)) {
          actionBtns.push(
            `<button class="btn btn-amber btn-xs row-arr-import" data-group-id="${escapeHtml(grp.id)}" type="button">Import to Arr</button>`
          );
        }
        if (arrMatches.some((m) => m.can_rescan)) {
          actionBtns.push(
            `<button class="btn btn-secondary btn-xs row-arr-rescan" data-group-id="${escapeHtml(grp.id)}" type="button">Rescan in Arr</button>`
          );
        }
        const erroredTorrents = grp.torrents.filter((t) => Boolean(t.tracker_health));
        if (erroredTorrents.length > 0) {
          const errBtnText = grp.has_unregistered ? "Delete Unregistered" : "Delete Errored";
          actionBtns.push(
            `<button class="btn btn-outline-danger btn-xs row-delete-errored-torrent" data-group-id="${escapeHtml(grp.id)}" type="button">${errBtnText}</button>`
          );
        }
        if (grp.torrents.length > 0) {
          actionBtns.push(
            `<button class="btn btn-danger btn-xs row-delete-torrent" data-group-id="${escapeHtml(grp.id)}" type="button">Delete Torrent</button>`
          );
        }
        if (grp.can_hardlink && grp.hardlink_pairs.length > 0) {
          actionBtns.push(
            `<button class="btn btn-amber btn-xs row-hardlink" data-group-id="${escapeHtml(grp.id)}" type="button">Make Hardlink</button>`
          );
        }
        if (grp.all_paths.length > 0 && (grp.seeding_summary === "UNTRACKED" || grp.status === "ORPHANED_IN_B")) {
          actionBtns.push(
            `<button class="btn btn-outline-danger btn-xs row-delete-file" data-group-id="${escapeHtml(grp.id)}" type="button">Delete File</button>`
          );
        }
        actionBtns.push(
          `<button class="btn btn-secondary btn-xs row-refresh" data-group-id="${escapeHtml(grp.id)}" type="button" title="Refresh disk, qBittorrent & Radarr/Sonarr status for this item">Refresh</button>`
        );

        const arrSyncHtml = state.pendingArrRefreshGroups.has(grp.id)
          ? `<div class="arr-sync-badge">Syncing with Radarr/Sonarr...</div>`
          : "";

        return `
          <tr data-group-id="${escapeHtml(grp.id)}">
            <td class="col-check">
              <input type="checkbox" class="row-checkbox" data-group-id="${escapeHtml(grp.id)}" ${checked} />
            </td>
            <td class="col-file">
              <div class="file-name">${escapeHtml(grp.filename)}</div>
              <div class="file-meta">
                <span>${formatBytes(grp.size)}</span>
                <span>${escapeHtml(grp.ext || "file")}</span>
              </div>
            </td>
            <td class="col-folders">
              ${renderStatusBadge(grp)}
              ${folderHtml}
            </td>
            <td class="col-qbit">
              ${renderSeedingBadge(grp)}
              ${torrentsHtml}
            </td>
            <td class="col-arr">
              ${renderArrBadge(grp.arr_summary)}
              ${arrHtml}
              ${arrSyncHtml}
            </td>
            <td class="col-reclaim">
              <div class="reclaim-bytes ${reclaimClass}">${formatBytes(grp.reclaimable_bytes)}</div>
              ${
                grp.status === "DUPLICATE_COPY" && !grp.can_hardlink
                  ? `<div class="torrent-sub">Cross-device (cannot link)</div>`
                  : ""
              }
            </td>
            <td class="col-actions">
              <div class="row-actions">${actionBtns.join("") || `<span class="torrent-sub">—</span>`}</div>
            </td>
          </tr>
        `;
      })
      .join("");

    updateSelectionControls(filtered);
  }

  function updateSelectionControls(filteredRows) {
    const visibleIds = new Set(filteredRows.map((g) => g.id));
    const selectedVisible = [...state.selectedGroupIds].filter((id) => visibleIds.has(id));
    const selectAll = $("selectAllCheckbox");
    selectAll.checked = filteredRows.length > 0 && selectedVisible.length === filteredRows.length;
    selectAll.indeterminate = selectedVisible.length > 0 && selectedVisible.length < filteredRows.length;

    const selectedGroups = getSelectedGroups();
    $("selectionBadge").textContent = `${selectedGroups.length} selected`;

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
      state.selectedGroupIds.clear();
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

  async function refreshGroupsInPlace(groupIds, waitDeletedPaths = [], includeArr = true) {
    const uniqueIds = [...new Set((groupIds || []).filter(Boolean))];
    if (!state.scan || uniqueIds.length === 0) return null;

    const epoch = state.scanEpoch;
    const res = await apiFetch("/api/scan/refresh-groups", {
      method: "POST",
      body: JSON.stringify({
        group_ids: uniqueIds,
        wait_for_deleted_paths: waitDeletedPaths,
        include_arr: includeArr,
      }),
    });
    if (epoch !== state.scanEpoch) return null;

    if (res.full_scan) {
      state.scan = res.full_scan;
      state.selectedGroupIds.clear();
      state.pendingArrRefreshGroups.clear();
      renderAll();
      return res;
    }

    const removedSet = new Set(res.removed_group_ids || []);
    const updatedMap = new Map((res.updated_groups || []).map((g) => [g.id, g]));

    for (const id of removedSet) {
      state.selectedGroupIds.delete(id);
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

  async function pollArrCommandsAndRefresh(commands, groupIds) {
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
      await refreshGroupsInPlace(uniqueIds, [], true);
      if (epoch !== state.scanEpoch) return;
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
      };
    }

    return {};
  }

  async function openActionModal(actionType, groups) {
    state.pendingAction = { type: actionType, groups };
    const titles = {
      "convert-hardlinks": "Convert Duplicate Copies to Hardlinks",
      "delete-torrents": "Delete Torrents from qBittorrent",
      "delete-files": "Delete Files from Disk",
      "arr-import": "Import Release into Radarr / Sonarr",
      "arr-rescan": "Rescan Library Item in Radarr / Sonarr",
      "arr-fix": "Fix Selected References in Radarr / Sonarr",
    };
    $("actionModalTitle").textContent = `${titles[actionType]} — Dry-Run Preview`;
    $("torrentDeleteOptionRow").hidden = actionType !== "delete-torrents";
    $("forceManualImportCheckbox").checked = false;
    const arrPayload = (actionType === "arr-import" || actionType === "arr-fix")
      ? buildActionPayloadForGroups(actionType, groups)
      : null;
    $("arrImportOptionRow").hidden = !(arrPayload && arrPayload.importItems.length > 0);
    const isArrAction = actionType.startsWith("arr-");
    $("confirmActionBtn").className = isArrAction ? "btn btn-primary" : "btn btn-danger";
    $("actionModal").hidden = false;
    await refreshActionDryRun();
  }

  async function refreshActionDryRun() {
    if (!state.pendingAction) return;
    const { type, groups } = state.pendingAction;
    const payload = buildActionPayloadForGroups(type, groups);
    const ro = state.config.read_only_mode;

    $("actionReadOnlyBanner").hidden = !ro;
    $("unlockAndExecuteBtn").hidden = !ro;
    $("confirmActionBtn").disabled = ro;
    $("actionPreviewSummary").textContent = "Running dry-run validation...";
    $("actionPreviewList").innerHTML = "";

    try {
      let result;
      if (type === "convert-hardlinks") {
        result = await apiFetch("/api/actions/convert-hardlinks", {
          method: "POST",
          body: JSON.stringify({ pairs: payload.pairs, dry_run: true }),
        });
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
              dry_run: true,
            }),
          });
          totalTriggered += impRes.triggered_count;
          summaryParts.push(`<strong>${impRes.triggered_count}</strong> import scan(s)`);
          for (const r of impRes.results) {
            const rejHtml = (r.rejections || []).length > 0
              ? `<br><small style="color:#fca5a5;">Rejections: ${escapeHtml(r.rejections.join("; "))}</small>`
              : "";
            const parsedHtml = r.parsed_title
              ? `<br><small>Arr parsed: ${escapeHtml(r.parsed_title)}</small>`
              : "";
            rowsHtml.push(
              `<div class="preview-row">[${escapeHtml(r.instance_name)}] ${escapeHtml(r.command_name)}: ${escapeHtml(r.remote_path)}${parsedHtml}${rejHtml}</div>`
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
      $("actionPreviewSummary").textContent = `Preview failed: ${err.message}`;
      $("confirmActionBtn").disabled = true;
    }
  }

  async function executePendingAction(unlockFirst = false) {
    if (!state.pendingAction) return;
    const { type, groups } = state.pendingAction;

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
        void pollArrCommandsAndRefresh(triggeredCommands, affectedIds);
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
    $("settingsModal").hidden = false;
  }

  function renderSettingsEditors() {
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
        syncActiveTabUi();
        renderTable();
      });
    });

    document.querySelectorAll("#kpiGrid .kpi-card").forEach((card) => {
      card.addEventListener("click", () => {
        state.activeTab = card.dataset.filterTab;
        syncActiveTabUi();
        renderTable();
      });
    });

    $("searchInput").addEventListener("input", (e) => {
      state.search = e.target.value;
      renderTable();
    });
    $("seedingFilter").addEventListener("change", (e) => {
      state.seedingFilter = e.target.value;
      renderTable();
    });
    $("arrFilter").addEventListener("change", (e) => {
      state.arrFilter = e.target.value;
      renderTable();
    });
    $("folderFilter").addEventListener("change", (e) => {
      state.folderFilter = e.target.value;
      renderTable();
    });

    $("selectAllCheckbox").addEventListener("change", (e) => {
      const filtered = getFilteredGroups();
      if (e.target.checked) {
        filtered.forEach((g) => state.selectedGroupIds.add(g.id));
      } else {
        filtered.forEach((g) => state.selectedGroupIds.delete(g.id));
      }
      renderTable();
    });

    $("auditTableBody").addEventListener("click", async (e) => {
      const target = e.target;
      if (target.classList.contains("row-checkbox")) {
        const gid = target.dataset.groupId;
        if (target.checked) state.selectedGroupIds.add(gid);
        else state.selectedGroupIds.delete(gid);
        updateSelectionControls(getFilteredGroups());
        return;
      }

      const btn = target.closest("button");
      if (!btn || !state.scan) return;
      const gid = btn.dataset.groupId;
      const grp = state.scan.groups.find((g) => g.id === gid);
      if (!grp) return;

      if (btn.classList.contains("row-arr-import")) {
        openActionModal("arr-import", [grp]);
      } else if (btn.classList.contains("row-arr-rescan")) {
        openActionModal("arr-rescan", [grp]);
      } else if (btn.classList.contains("row-delete-errored-torrent")) {
        openActionModal("delete-torrents", [
          { ...grp, torrents: grp.torrents.filter((t) => Boolean(t.tracker_health)) },
        ]);
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
            btn.textContent = "Refresh";
          }
        }
      }
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
    $("forceManualImportCheckbox").addEventListener("change", refreshActionDryRun);
    $("confirmActionBtn").addEventListener("click", () => executePendingAction(false));
    $("unlockAndExecuteBtn").addEventListener("click", () => executePendingAction(true));

    $("closeSettingsBtn").addEventListener("click", () => ($("settingsModal").hidden = true));
    $("cancelSettingsBtn").addEventListener("click", () => ($("settingsModal").hidden = true));
    $("saveSettingsBtn").addEventListener("click", saveSettingsAndScan);

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
