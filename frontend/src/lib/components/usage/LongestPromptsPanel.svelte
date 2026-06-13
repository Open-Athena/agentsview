<script lang="ts">
  import { usage } from "../../stores/usage.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";

  function truncatePreview(s: string, max = 100): string {
    const oneLine = s.replace(/\s+/g, " ").trim();
    if (oneLine.length <= max) return oneLine;
    return oneLine.slice(0, max - 1) + "…";
  }

  // Navigate then queue a scroll-to-ordinal: the session view
  // mounts, reads ui.pendingScrollOrdinal, and jumps to the
  // matching message. Same machinery the in-session search uses.
  function handleRowClick(sessionId: string, ordinal: number) {
    router.navigateToSession(sessionId);
    ui.scrollToOrdinal(ordinal, sessionId);
  }

  function fmtChars(n: number): string {
    if (n >= 1000) return `${(n / 1000).toFixed(1)}k`;
    return String(n);
  }

  // Header text reflects whether a project filter is active so
  // it's obvious the panel is project-scoped, not global.
  const headerSuffix = $derived(
    sessions.filters.project
      ? ` — ${sessions.filters.project}`
      : "",
  );
</script>

<div class="longest-prompts-container">
  <h3 class="chart-title">
    Longest User Prompts{headerSuffix}
  </h3>

  {#if usage.errors.longestPrompts}
    <div class="error">
      {usage.errors.longestPrompts}
      <button
        class="retry-btn"
        onclick={() => usage.fetchLongestPrompts()}
      >
        Retry
      </button>
    </div>
  {:else if usage.longestPrompts && usage.longestPrompts.length > 0}
    <div class="prompt-list">
      {#each usage.longestPrompts as row, i (row.session_id + ":" + row.ordinal)}
        <!-- svelte-ignore a11y_click_events_have_key_events -->
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <div
          class="prompt-row"
          title={row.preview}
          onclick={() => handleRowClick(row.session_id, row.ordinal)}
        >
          <span class="rank">{i + 1}</span>
          <span class="chars">{fmtChars(row.content_length)}</span>
          <div class="prompt-info">
            <span class="prompt-preview">
              {truncatePreview(row.preview, 140)}
            </span>
            <span class="prompt-meta">
              {row.project} &middot; #{row.ordinal} &middot; {row.session_id.slice(0, 8)}
            </span>
          </div>
        </div>
      {/each}
    </div>
  {:else}
    <div class="empty">No prompts in range</div>
  {/if}
</div>

<style>
  .longest-prompts-container {
    flex: 1;
    display: flex;
    flex-direction: column;
  }

  .chart-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
    margin-bottom: 8px;
  }

  .prompt-list {
    display: flex;
    flex-direction: column;
    gap: 2px;
    overflow-y: auto;
    flex: 1;
  }

  .prompt-row {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 4px 6px;
    border-radius: var(--radius-sm);
    cursor: pointer;
    transition: background 0.1s;
  }

  .prompt-row:hover {
    background: var(--bg-surface-hover);
  }

  .rank {
    flex-shrink: 0;
    width: 18px;
    text-align: right;
    font-size: 10px;
    font-weight: 600;
    color: var(--text-muted);
    font-family: var(--font-mono);
  }

  .chars {
    flex-shrink: 0;
    width: 44px;
    text-align: right;
    font-size: 11px;
    font-weight: 500;
    font-family: var(--font-mono);
    color: var(--accent-blue);
  }

  .prompt-info {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 1px;
  }

  .prompt-preview {
    font-size: 11px;
    color: var(--text-secondary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .prompt-meta {
    font-size: 9px;
    color: var(--text-muted);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    font-family: var(--font-mono);
  }

  .empty {
    color: var(--text-muted);
    font-size: 12px;
    padding: 24px;
    text-align: center;
  }

  .error {
    color: var(--accent-red);
    font-size: 12px;
    padding: 12px;
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .retry-btn {
    padding: 2px 8px;
    border: 1px solid currentColor;
    border-radius: var(--radius-sm);
    font-size: 11px;
    color: inherit;
    cursor: pointer;
  }
</style>
