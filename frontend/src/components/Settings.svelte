<script>
  import { createEventDispatcher, onMount } from 'svelte';
  import { settings } from '../lib/settings.js';
  import { errorText } from '../lib/errors.js';
  import { groupByProvider } from '../lib/format.js';
  import {
    AddCustomProvider,
    AllModelList,
    ConnectProvider,
    DisconnectProvider,
    DiscoverCustomProvider,
    GenerateAPIKeyEnvName,
    GetRuntimeConfig,
    ProviderList,
    RemoveProvider,
    SetDefaultModel,
    SetModelHidden,
    SetProviderHidden,
    SetRuntimeConfig,
  } from '../../wailsjs/go/main/App';
  const dispatch = createEventDispatcher();
  export let initialSection = 'appearance';

  const sections = [
    { id: 'appearance', label: 'Appearance' },
    { id: 'models', label: 'Models' },
    { id: 'providers', label: 'Providers' },
    { id: 'runtime', label: 'Runtime' },
  ];
  let active = initialSection;

  let prevInitial = initialSection;
  $: if (initialSection !== prevInitial) { active = initialSection; prevInitial = initialSection; }

  function toggleWrap(e) {
    settings.update((s) => ({ ...s, wrapCode: e.target.checked }));
  }

  function setFontScale(e) {
    const n = parseInt(e.target.value, 10);
    if (!Number.isFinite(n)) return;
    const clamped = Math.max(50, Math.min(200, n));
    settings.update((s) => ({ ...s, fontScale: clamped }));
  }

  function stepFontScale(delta) {
    settings.update((s) => {
      const next = Math.max(50, Math.min(200, (s.fontScale || 100) + delta));
      return { ...s, fontScale: next };
    });
  }

  let allModels = [];
  let modelGroups = [];
  let modelQuery = '';

  let providers = [];
  let providersLoading = false;
  let connectTarget = null;
  let connectKey = '';
  let connectBusy = false;

  let showCustomModal = false;
  let customBusy = false;
  let customError = '';
  let customForm = emptyCustomForm();
  let customHeadersText = '{}';
  let customOptionsText = '{}';
  let customExtraBodyText = '{}';
  let candidates = [];
  let selectedModels = [];

  let runtimeForm = emptyRuntimeForm();
  let runtimeLoading = false;
  let runtimeSaving = false;

  $: filteredGroups = filterModelGroups(modelGroups, modelQuery);

  function filterModelGroups(groups, q) {
    if (!q.trim()) return groups;
    const lq = q.toLowerCase();
    return groups.map(g => {
      const models = g.models.filter(e =>
        (e.displayName || '').toLowerCase().includes(lq) ||
        (e.model || '').toLowerCase().includes(lq) ||
        (e.ref || '').toLowerCase().includes(lq)
      );
      return { ...g, models };
    }).filter(g => g.models.length > 0);
  }

  onMount(async () => {
    await Promise.all([refreshModels(), refreshProviders(), refreshRuntimeConfig()]);
  });

  async function refreshModels() {
    try { allModels = await AllModelList(); } catch (e) { dispatch('error', errorText(e)); allModels = []; }
    modelGroups = groupByProvider(allModels);
  }

  async function refreshProviders() {
    providersLoading = true;
    try { providers = await ProviderList(); } catch (e) { dispatch('error', errorText(e)); providers = []; }
    providersLoading = false;
  }

  async function refreshConfigurationViews() {
    await Promise.all([refreshProviders(), refreshModels()]);
  }

  function modelDisplayName(entry) {
    return entry.displayName || entry.model || entry.ref;
  }

  async function toggleModel(entry, e) {
    const hidden = !e.target.checked;
    try {
      await SetModelHidden(entry.ref, hidden);
      await refreshModels();
    } catch (err) { dispatch('error', errorText(err)); }
  }

  async function toggleProvider(group, e) {
    const hidden = !e.target.checked;
    try {
      await SetProviderHidden(group.provider, hidden);
      await refreshModels();
    } catch (err) { dispatch('error', errorText(err)); }
  }

  async function setDefaultModel(entry) {
    try {
      await SetDefaultModel(entry.ref);
      await refreshModels();
    } catch (err) { dispatch('error', errorText(err)); }
  }

  function emptyRuntimeForm() {
    return {
      sessions: { archive_after_days: 7, delete_after_archive_days: 7 },
      compaction: { threshold_pct: 0.9, summarizer_model: '' },
      subagents: { max_concurrent: 4, model: '' },
      tools: {
        max_output_bytes: 15360,
        read_max_lines: 500,
        read_line_max_chars: 5000,
        command_timeout: 120,
        max_background_processes: 10,
      },
    };
  }

  async function refreshRuntimeConfig() {
    runtimeLoading = true;
    try { runtimeForm = await GetRuntimeConfig(); }
    catch (err) { dispatch('error', errorText(err)); runtimeForm = emptyRuntimeForm(); }
    runtimeLoading = false;
  }

  function setRuntimeNumber(section, field, value, float = false) {
    const n = float ? parseFloat(value) : parseInt(value, 10);
    runtimeForm = {
      ...runtimeForm,
      [section]: { ...runtimeForm[section], [field]: Number.isFinite(n) ? n : 0 },
    };
  }

  function setRuntimeText(section, field, value) {
    runtimeForm = { ...runtimeForm, [section]: { ...runtimeForm[section], [field]: value } };
  }

  async function saveRuntimeConfig() {
    runtimeSaving = true;
    try {
      await SetRuntimeConfig(runtimeForm);
      await refreshRuntimeConfig();
    } catch (err) { dispatch('error', errorText(err)); }
    runtimeSaving = false;
  }

  function providerStatusText(provider) {
    if (provider.connected) {
      if (provider.keySource === 'managed') return 'Connected · Lightcode-managed key';
      if (provider.keySource === 'external') return 'Connected · environment key';
      if (provider.keySource === 'keyless') return 'Connected · keyless';
      return 'Connected';
    }
    if (provider.keySource === 'external') return 'Environment key available · discovery needed';
    if (provider.keySource === 'keyless') return 'Keyless, not connected';
    return 'Not connected';
  }

  function providerActionNote(provider) {
    if (provider.keySource === 'external') return `Key comes from ${provider.apiKeyEnv}; unset it outside Lightcode to disconnect.`;
    if (provider.keySource === 'none' && provider.apiKeyEnv) return `Uses ${provider.apiKeyEnv}. Keys are write-only and never displayed.`;
    if (provider.keySource === 'managed') return `Uses ${provider.apiKeyEnv}.`;
    if (provider.keySource === 'keyless') return 'No API key required.';
    return '';
  }

  function openConnect(provider) {
    connectTarget = provider;
    connectKey = '';
  }

  function cancelConnect() {
    connectTarget = null;
    connectKey = '';
    connectBusy = false;
  }

  async function submitConnect() {
    if (!connectTarget) return;
    connectBusy = true;
    try {
      await ConnectProvider(connectTarget.id, connectKey);
      cancelConnect();
      await refreshConfigurationViews();
    } catch (err) {
      dispatch('error', errorText(err));
      connectKey = '';
      connectBusy = false;
    }
  }

  async function connectKeyless(provider) {
    try {
      await ConnectProvider(provider.id, '');
      await refreshConfigurationViews();
    } catch (err) { dispatch('error', errorText(err)); }
  }

  async function connectWithExistingKey(provider) {
    try {
      await ConnectProvider(provider.id, '');
      await refreshConfigurationViews();
    } catch (err) { dispatch('error', errorText(err)); }
  }

  async function disconnect(provider) {
    try {
      await DisconnectProvider(provider.id);
      await refreshConfigurationViews();
    } catch (err) { dispatch('error', errorText(err)); }
  }

  async function remove(provider) {
    try {
      await RemoveProvider(provider.id);
      await refreshConfigurationViews();
    } catch (err) { dispatch('error', errorText(err)); }
  }

  function emptyCustomForm() {
    return { id: '', name: '', baseURL: '', apiKeyEnv: '', apiKey: '', discovery: true, systemRole: '', usageInStream: null, maxTokensField: '', hidden: false };
  }

  function openCustomModal() {
    customForm = emptyCustomForm();
    customHeadersText = '{}';
    customOptionsText = '{}';
    customExtraBodyText = '{}';
    candidates = [];
    selectedModels = [];
    customError = '';
    customBusy = false;
    showCustomModal = true;
  }

  function cancelCustom() {
    showCustomModal = false;
    customForm.apiKey = '';
    customForm = emptyCustomForm();
    customHeadersText = '{}';
    customOptionsText = '{}';
    customExtraBodyText = '{}';
    candidates = [];
    selectedModels = [];
    customError = '';
    customBusy = false;
  }

  async function fillGeneratedEnvName() {
    if (!customForm.id.trim()) return;
    try { customForm.apiKeyEnv = await GenerateAPIKeyEnvName(customForm.id.trim()); }
    catch (err) { dispatch('error', errorText(err)); }
  }

  function parseJSONObject(text, label) {
    const trimmed = text.trim();
    if (!trimmed) return {};
    let parsed;
    try { parsed = JSON.parse(trimmed); }
    catch (err) { throw new Error(`${label} must be valid JSON`); }
    if (parsed === null || Array.isArray(parsed) || typeof parsed !== 'object') throw new Error(`${label} must be a JSON object`);
    return parsed;
  }

  function customRequest(models = selectedModels) {
    const req = {
      id: customForm.id.trim(),
      name: customForm.name.trim(),
      baseURL: customForm.baseURL.trim(),
      apiKeyEnv: customForm.apiKeyEnv.trim(),
      apiKey: customForm.apiKey,
      discovery: customForm.discovery,
      models,
    };
    const headers = parseJSONObject(customHeadersText, 'Headers');
    const options = parseJSONObject(customOptionsText, 'Transport options');
    const extraBody = parseJSONObject(customExtraBodyText, 'Extra body');
    if (Object.keys(headers).length) req.headers = headers;
    if (Object.keys(options).length) req.options = options;
    if (Object.keys(extraBody).length) req.extraBody = extraBody;
    if (customForm.systemRole.trim()) req.systemRole = customForm.systemRole.trim();
    if (customForm.usageInStream !== null && customForm.usageInStream !== undefined) req.usageInStream = customForm.usageInStream;
    if (customForm.maxTokensField.trim()) req.maxTokensField = customForm.maxTokensField.trim();
    if (customForm.hidden) req.hidden = true;
    return req;
  }

  async function runDiscovery() {
    customError = '';
    customBusy = true;
    try {
      const discovered = await DiscoverCustomProvider(customRequest([]));
      candidates = discovered || [];
      selectedModels = candidates.filter(c => c.usable).map(candidateToModel);
    } catch (err) {
      customError = errorText(err);
      customForm.apiKey = '';
    }
    customBusy = false;
  }

  function candidateToModel(candidate) {
    return {
      id: candidate.id || '',
      name: candidate.name || candidate.id || '',
      contextWindow: candidate.contextWindow || 0,
      maxOutputTokens: candidate.maxOutputTokens || 0,
      cost: candidate.cost,
      systemRole: '',
      usageInStream: null,
      hidden: false,
    };
  }

  function addCandidate(candidate) {
    if (selectedModels.some(m => m.id === candidate.id)) return;
    selectedModels = [...selectedModels, candidateToModel(candidate)];
  }

  function addBlankModel() {
    selectedModels = [...selectedModels, { id: '', name: '', contextWindow: 0, maxOutputTokens: 0, systemRole: '', usageInStream: null, hidden: false }];
  }

  function removeSelectedModel(index) {
    selectedModels = selectedModels.filter((_, i) => i !== index);
  }

  function updateSelectedModel(index, field, value) {
    selectedModels = selectedModels.map((model, i) => {
      if (i !== index) return model;
      if (field === 'contextWindow' || field === 'maxOutputTokens') {
        const n = parseInt(value, 10);
        return { ...model, [field]: Number.isFinite(n) ? n : 0 };
      }
      if (field === 'hidden') {
        return { ...model, hidden: !!value };
      }
      if (field === 'usageInStream') {
        return { ...model, usageInStream: value };
      }
      return { ...model, [field]: value };
    });
  }

  async function submitCustom() {
    customError = '';
    customBusy = true;
    try {
      await AddCustomProvider(customRequest(selectedModels));
      cancelCustom();
      await refreshConfigurationViews();
    } catch (err) {
      customError = errorText(err);
      customForm.apiKey = '';
      customBusy = false;
    }
  }
</script>

<div class="layer">
  <button type="button" class="backdrop" tabindex="-1" aria-label="Close settings" on:click={() => dispatch('close')}></button>
  <div class="prompt" role="dialog" aria-modal="true" aria-labelledby="settings-title" tabindex="-1">
    <div class="hdr" id="settings-title">Settings</div>
    <div class="body">
      <div class="sidebar">
        {#each sections as s}
          <button class="nav-item" class:active={active === s.id} on:click={() => active = s.id}>{s.label}</button>
        {/each}
      </div>
      <div class="content">
        {#if active === 'appearance'}
          <div class="section-title">Appearance</div>
          <label class="option">
            <span>Wrap code lines</span>
            <span class="switch">
              <input type="checkbox" checked={$settings.wrapCode} on:change={toggleWrap} />
              <span class="track"><span class="thumb"></span></span>
            </span>
          </label>
          <div class="option">
            <span>Message font scale (%)</span>
            <span class="stepper">
              <button type="button" class="step" on:click={() => stepFontScale(-10)} disabled={$settings.fontScale <= 50}>−</button>
              <input type="number" min="50" max="200" step="10" value={$settings.fontScale} on:change={setFontScale} class="num" />
              <button type="button" class="step" on:click={() => stepFontScale(10)} disabled={$settings.fontScale >= 200}>+</button>
            </span>
          </div>
        {/if}
        {#if active === 'models'}
          <div class="section-title">Models</div>
          <p class="placeholder">Toggle model visibility in the model selector. Hidden models are still available by ref.</p>
          <input class="model-search" type="text" placeholder="Filter models..." bind:value={modelQuery} />
          {#each filteredGroups as group (group.provider)}
            <div class="models-group">
              <label class="option provider-row">
                <span>{group.providerName}</span>
                <span class="switch">
                  <input type="checkbox" checked={!group.providerHidden} on:change={(e) => toggleProvider(group, e)} />
                  <span class="track"><span class="thumb"></span></span>
                </span>
              </label>
              {#each group.models as entry (entry.ref)}
                <div class="option model-row" class:dimmed={group.providerHidden}>
                  <span>
                    {modelDisplayName(entry)}
                    {#if entry.default}<small class="default-tag">Default</small>{/if}
                  </span>
                  <span class="model-actions">
                    <button class="btn compact" type="button" disabled={entry.default} on:click={() => setDefaultModel(entry)}>Set default</button>
                    <span class="switch">
                      <input type="checkbox" checked={!entry.hidden} disabled={group.providerHidden} on:change={(e) => toggleModel(entry, e)} />
                      <span class="track"><span class="thumb"></span></span>
                    </span>
                  </span>
                </div>
              {/each}
            </div>
          {/each}
        {/if}
        {#if active === 'providers'}
          <div class="section-heading">
            <div>
              <div class="section-title">Providers</div>
              <p class="placeholder">Connect providers and manage custom OpenAI-compatible endpoints. API keys are write-only.</p>
            </div>
            <button class="btn" type="button" on:click={openCustomModal}>Add custom provider</button>
          </div>
          {#if providersLoading}
            <p class="placeholder">Loading providers...</p>
          {:else}
            {#each providers as provider (provider.id)}
              <div class="provider-card">
                <div class="provider-main">
                  <div>
                    <div class="provider-name">{provider.name || provider.id}</div>
                    <div class="provider-id">{provider.id}{provider.builtin ? ' · built-in' : ' · custom'}</div>
                  </div>
                  <span class:ok={provider.connected} class="status-pill">{providerStatusText(provider)}</span>
                </div>
                <div class="provider-meta">
                  <span>{provider.usableModels} usable / {provider.modelCount} models</span>
                  {#if provider.baseURL}<span>{provider.baseURL}</span>{/if}
                </div>
                {#if providerActionNote(provider)}<p class="provider-note">{providerActionNote(provider)}</p>{/if}
                <div class="provider-actions">
                  {#if provider.apiKeyEnv}
                    {#if !provider.connected && (provider.keySource === 'managed' || provider.keySource === 'external')}
                      <button class="btn" type="button" on:click={() => connectWithExistingKey(provider)}>Connect</button>
                    {:else}
                      <button class="btn" type="button" disabled={provider.connected && provider.keySource !== 'none'} on:click={() => openConnect(provider)}>Connect</button>
                    {/if}
                  {:else}
                    <button class="btn" type="button" disabled={provider.connected} on:click={() => connectKeyless(provider)}>Connect</button>
                  {/if}
                  <button class="btn" type="button" disabled={!provider.disconnectable} on:click={() => disconnect(provider)}>Disconnect</button>
                  <button class="btn" type="button" disabled={!provider.removable} on:click={() => remove(provider)}>Remove</button>
                </div>
              </div>
            {/each}
          {/if}
        {/if}
        {#if active === 'runtime'}
          <div class="section-heading">
            <div>
              <div class="section-title">Runtime</div>
              <p class="placeholder">Edit runtime settings stored in config.json.</p>
            </div>
            <button class="btn" type="button" disabled={runtimeSaving || runtimeLoading} on:click={saveRuntimeConfig}>Save</button>
          </div>
          {#if runtimeLoading}
            <p class="placeholder">Loading runtime config...</p>
          {:else}
            <div class="runtime-group">
              <div class="provider-row runtime-title">Sessions</div>
              <label class="runtime-field">Archive after days<input class="model-search" type="number" min="1" max="365" value={runtimeForm.sessions.archive_after_days} on:input={(e) => setRuntimeNumber('sessions', 'archive_after_days', e.target.value)} /></label>
              <label class="runtime-field">Delete after archive days<input class="model-search" type="number" min="1" max="365" value={runtimeForm.sessions.delete_after_archive_days} on:input={(e) => setRuntimeNumber('sessions', 'delete_after_archive_days', e.target.value)} /></label>
            </div>
            <div class="runtime-group">
              <div class="provider-row runtime-title">Compaction</div>
              <label class="runtime-field">Threshold percent<input class="model-search" type="number" min="0.1" max="0.99" step="0.01" value={runtimeForm.compaction.threshold_pct} on:input={(e) => setRuntimeNumber('compaction', 'threshold_pct', e.target.value, true)} /></label>
              <label class="runtime-field">Summarizer model<input class="model-search" type="text" placeholder="provider/model or empty" value={runtimeForm.compaction.summarizer_model} on:input={(e) => setRuntimeText('compaction', 'summarizer_model', e.target.value)} /></label>
            </div>
            <div class="runtime-group">
              <div class="provider-row runtime-title">Subagents</div>
              <label class="runtime-field">Max concurrent<input class="model-search" type="number" min="1" max="20" value={runtimeForm.subagents.max_concurrent} on:input={(e) => setRuntimeNumber('subagents', 'max_concurrent', e.target.value)} /></label>
              <label class="runtime-field">Model<input class="model-search" type="text" placeholder="provider/model or empty" value={runtimeForm.subagents.model} on:input={(e) => setRuntimeText('subagents', 'model', e.target.value)} /></label>
            </div>
            <div class="runtime-group">
              <div class="provider-row runtime-title">Tools</div>
              <label class="runtime-field">Max output bytes<input class="model-search" type="number" min="1024" max="1048576" value={runtimeForm.tools.max_output_bytes} on:input={(e) => setRuntimeNumber('tools', 'max_output_bytes', e.target.value)} /></label>
              <label class="runtime-field">Read max lines<input class="model-search" type="number" min="10" max="10000" value={runtimeForm.tools.read_max_lines} on:input={(e) => setRuntimeNumber('tools', 'read_max_lines', e.target.value)} /></label>
              <label class="runtime-field">Read line max chars<input class="model-search" type="number" min="100" max="100000" value={runtimeForm.tools.read_line_max_chars} on:input={(e) => setRuntimeNumber('tools', 'read_line_max_chars', e.target.value)} /></label>
              <label class="runtime-field">Command timeout seconds<input class="model-search" type="number" min="5" max="600" value={runtimeForm.tools.command_timeout} on:input={(e) => setRuntimeNumber('tools', 'command_timeout', e.target.value)} /></label>
              <label class="runtime-field">Max background processes<input class="model-search" type="number" min="1" max="50" value={runtimeForm.tools.max_background_processes} on:input={(e) => setRuntimeNumber('tools', 'max_background_processes', e.target.value)} /></label>
            </div>
          {/if}
        {/if}
      </div>
    </div>
    <div class="actions">
      <button class="btn" on:click={() => dispatch('close')}>Close</button>
    </div>
  </div>

  {#if connectTarget}
    <div class="modal-card" role="dialog" aria-modal="true" aria-labelledby="connect-title">
      <div class="hdr" id="connect-title">Connect {connectTarget.name || connectTarget.id}</div>
      <div class="modal-body">
        <p class="placeholder">Enter the API key for {connectTarget.apiKeyEnv}. It will not be displayed again.</p>
        <input class="model-search" type="password" autocomplete="off" placeholder="API key" bind:value={connectKey} />
      </div>
      <div class="actions">
        <button class="btn" type="button" on:click={cancelConnect}>Cancel</button>
        <button class="btn" type="button" disabled={connectBusy || !connectKey.trim()} on:click={submitConnect}>Connect</button>
      </div>
    </div>
  {/if}

  {#if showCustomModal}
    <div class="modal-card custom-modal" role="dialog" aria-modal="true" aria-labelledby="custom-title">
      <div class="hdr" id="custom-title">Add custom provider</div>
      <div class="modal-body custom-body">
        {#if customError}<p class="error-text">{customError}</p>{/if}
        <div class="form-grid">
          <label>Provider ID<input class="model-search" type="text" bind:value={customForm.id} placeholder="my-provider" /></label>
          <label>Name<input class="model-search" type="text" bind:value={customForm.name} placeholder="My Provider" /></label>
          <label>Base URL<input class="model-search" type="text" bind:value={customForm.baseURL} placeholder="https://example.com/v1" /></label>
          <label>API key env<input class="model-search" type="text" bind:value={customForm.apiKeyEnv} placeholder="LIGHTCODE_MY_PROVIDER_API_KEY" /></label>
        </div>
        <div class="inline-actions">
          <button class="btn" type="button" on:click={fillGeneratedEnvName} disabled={!customForm.id.trim()}>Generate env name</button>
        </div>
        <label>API key<input class="model-search" type="password" autocomplete="off" bind:value={customForm.apiKey} placeholder="Write-only key value" /></label>
        <details class="advanced">
          <summary>Advanced provider fields</summary>
          <label>System role<select class="model-search" bind:value={customForm.systemRole}>
            <option value="">Default (system)</option>
            <option value="system">system</option>
            <option value="developer">developer</option>
          </select></label>
          <label>Usage in stream<select class="model-search" value={customForm.usageInStream === null ? 'auto' : String(customForm.usageInStream)} on:change={(e) => { customForm.usageInStream = e.target.value === 'auto' ? null : e.target.value === 'true'; }}>
            <option value="auto">Auto-detect</option>
            <option value="true">true</option>
            <option value="false">false</option>
          </select></label>
          <label>Max tokens field<input class="model-search" type="text" bind:value={customForm.maxTokensField} placeholder="max_completion_tokens" /></label>
          <label class="option"><span>Hidden</span><input type="checkbox" bind:checked={customForm.hidden} /></label>
          <label>Transport headers JSON<textarea bind:value={customHeadersText}></textarea></label>
          <label>Transport options JSON<textarea bind:value={customOptionsText}></textarea></label>
          <label>Provider extra_body JSON<textarea bind:value={customExtraBodyText}></textarea></label>
        </details>
        <div class="inline-actions">
          <button class="btn" type="button" disabled={customBusy || !customForm.baseURL.trim()} on:click={runDiscovery}>Discover models</button>
          <button class="btn" type="button" on:click={addBlankModel}>Add model manually</button>
        </div>
        {#if candidates.length}
          <div class="section-title nested-title">Discovered models</div>
          {#each candidates as candidate (candidate.id)}
            <div class="candidate-row">
              <span>{candidate.name || candidate.id}<small>{candidate.contextWindow || 0} context</small></span>
              <button class="btn" type="button" disabled={!candidate.usable || selectedModels.some(m => m.id === candidate.id)} on:click={() => addCandidate(candidate)}>+</button>
            </div>
          {/each}
        {/if}
        <div class="section-title nested-title">Models</div>
        {#if !selectedModels.length}<p class="placeholder">Add at least one usable model.</p>{/if}
        {#each selectedModels as model, index}
          <div class="model-editor">
            <input class="model-search" type="text" placeholder="model id" value={model.id} on:input={(e) => updateSelectedModel(index, 'id', e.target.value)} />
            <input class="model-search" type="text" placeholder="display name" value={model.name} on:input={(e) => updateSelectedModel(index, 'name', e.target.value)} />
            <input class="model-search" type="number" min="0" placeholder="context" value={model.contextWindow} on:input={(e) => updateSelectedModel(index, 'contextWindow', e.target.value)} />
            <input class="model-search" type="number" min="0" placeholder="max output" value={model.maxOutputTokens} on:input={(e) => updateSelectedModel(index, 'maxOutputTokens', e.target.value)} />
            <details class="model-advanced">
              <summary>Advanced</summary>
              <select class="model-search" value={model.systemRole || ''} on:change={(e) => updateSelectedModel(index, 'systemRole', e.target.value)}>
                <option value="">Default system role</option>
                <option value="system">system</option>
                <option value="developer">developer</option>
              </select>
              <select class="model-search" value={model.usageInStream === null ? 'auto' : String(model.usageInStream)} on:change={(e) => updateSelectedModel(index, 'usageInStream', e.target.value === 'auto' ? null : e.target.value === 'true')}>
                <option value="auto">Usage in stream: auto</option>
                <option value="true">Usage in stream: true</option>
                <option value="false">Usage in stream: false</option>
              </select>
              <label class="option"><span>Hidden</span><input type="checkbox" checked={model.hidden || false} on:change={(e) => updateSelectedModel(index, 'hidden', e.target.checked)} /></label>
            </details>
            <button class="btn" type="button" on:click={() => removeSelectedModel(index)}>Remove</button>
          </div>
        {/each}
      </div>
      <div class="actions">
        <button class="btn" type="button" on:click={cancelCustom}>Cancel</button>
        <button class="btn" type="button" disabled={customBusy || !selectedModels.some(m => m.id.trim() && m.contextWindow > 0)} on:click={submitCustom}>Add provider</button>
      </div>
    </div>
  {/if}
</div>

<style>
  .layer { position:fixed; inset:0; z-index:300; display:flex; align-items:center; justify-content:center; }
  .backdrop { position:absolute; inset:0; border:0; padding:0; margin:0; background:var(--overlay); cursor:default; }
  .prompt { position:relative; z-index:1; background:var(--bg-elevated); border:1px solid var(--border-strong); min-width:560px; max-width:720px; height:88vh; max-height:88vh; display:flex; flex-direction:column; }
  .hdr { padding:8px 12px; font-size:12px; font-weight:600; text-transform:uppercase; letter-spacing:.5px; border-bottom:1px solid var(--border); }
  .body { display:flex; flex:1; min-height:0; }
  .sidebar { width:140px; border-right:1px solid var(--border); padding:8px 0; display:flex; flex-direction:column; overflow-y:auto; }
  .nav-item { background:none; border:none; color:var(--text-dim); font-family:var(--font-ui); font-size:12px; padding:6px 12px; cursor:pointer; text-align:left; }
  .nav-item:hover { color:var(--text); }
  .nav-item.active { color:var(--accent); background:var(--accent-soft); }
  .content { flex:1; padding:12px; overflow-y:auto; }
  .section-title { font-size:12px; font-weight:600; text-transform:uppercase; letter-spacing:.5px; color:var(--text); margin-bottom:8px; }
  .section-heading { display:flex; align-items:flex-start; justify-content:space-between; gap:12px; margin-bottom:8px; }
  .placeholder { font-family:var(--font-ui); font-size:12px; color:var(--text-dim); margin:0 0 8px; }
  .model-search { width:100%; padding:6px 12px; background:var(--bg-input); border:none; border-bottom:1px solid var(--border); color:var(--text); font-family:var(--font-ui); font-size:12px; outline:none; box-sizing:border-box; margin-bottom:8px; }
  .model-search::placeholder { color:var(--text-dim); }
  .model-search:focus { background:var(--bg-input-focus); }
  .option { display:flex; align-items:center; justify-content:space-between; font-family:var(--font-ui); font-size:12px; color:var(--text); cursor:pointer; padding:6px 0; min-height:28px; }
  .option + .option { border-top:1px solid var(--border); }
  .models-group + .models-group { margin-top:12px; }
  .provider-row { color:var(--text); font-weight:600; border-top:1px solid var(--border); }
  .model-row { padding-left:16px; }
  .model-row.dimmed { opacity:.4; }
  .model-actions { display:inline-flex; align-items:center; gap:8px; }
  .btn.compact { padding:2px 8px; }
  .default-tag { color:var(--accent); margin-left:8px; font-size:11px; }
  .switch { position:relative; display:inline-block; width:32px; height:18px; flex-shrink:0; }
  .switch input { position:absolute; inset:0; opacity:0; cursor:pointer; margin:0; }
  .switch .track { position:absolute; inset:0; background:var(--bg-input); border:1px solid var(--border-button); border-radius:999px; transition:background .15s, border-color .15s; }
  .switch .thumb { position:absolute; top:2px; left:2px; width:12px; height:12px; background:var(--text-dim); border-radius:50%; transition:transform .15s, background .15s; }
  .switch input:hover + .track { border-color:var(--accent); }
  .switch input:disabled + .track { cursor:default; }
  .switch input:disabled:hover + .track { border-color:var(--border-button); }
  .switch input:checked + .track { background:var(--accent-soft); border-color:var(--accent); }
  .switch input:checked + .track .thumb { transform:translateX(14px); background:var(--accent); }
  .stepper { display:inline-flex; align-items:stretch; }
  .stepper .step { width:22px; background:transparent; border:1px solid var(--border-button); color:var(--text-dim); font-family:var(--font-ui); font-size:13px; line-height:1; cursor:pointer; padding:0; }
  .stepper .step:hover:not(:disabled) { border-color:var(--accent); color:var(--accent); }
  .stepper .step:disabled { opacity:.4; cursor:default; }
  .stepper .step:first-child { border-right:none; }
  .stepper .step:last-child { border-left:none; }
  .option .num { width:48px; background:transparent; border:1px solid var(--border-button); color:var(--text); font-family:var(--font-ui); font-size:12px; padding:2px 6px; text-align:center; appearance:textfield; -moz-appearance:textfield; }
  .option .num::-webkit-outer-spin-button, .option .num::-webkit-inner-spin-button { -webkit-appearance:none; margin:0; }
  .option .num:focus { outline:none; border-color:var(--accent); position:relative; z-index:1; }
  .actions { display:flex; gap:8px; padding:8px 12px; border-top:1px solid var(--border); justify-content:flex-end; }
  .btn { padding:4px 12px; font-size:12px; cursor:pointer; border:1px solid var(--border-button); background:none; color:var(--text-dim); font-family:var(--font-ui); }
  .btn:hover:not(:disabled) { border-color:var(--accent); color:var(--text); }
  .btn:disabled { opacity:.4; cursor:default; }
  .provider-card { border-top:1px solid var(--border); padding:10px 0; font-family:var(--font-ui); font-size:12px; }
  .provider-main { display:flex; align-items:flex-start; justify-content:space-between; gap:12px; }
  .provider-name { color:var(--text); font-weight:600; }
  .provider-id, .provider-meta, .provider-note { color:var(--text-dim); }
  .provider-meta { display:flex; flex-wrap:wrap; gap:8px; margin-top:6px; }
  .provider-note { margin:6px 0 0; }
  .provider-actions { display:flex; gap:8px; margin-top:8px; }
  .runtime-group { border-top:1px solid var(--border); padding-top:8px; margin-top:12px; }
  .runtime-title { padding-bottom:6px; }
  .runtime-field { margin-bottom:8px; }
  .status-pill { color:var(--text-dim); border:1px solid var(--border); padding:2px 6px; white-space:nowrap; }
  .status-pill.ok { color:var(--accent); border-color:var(--accent); background:var(--accent-soft); }
  .modal-card { position:absolute; z-index:2; width:420px; max-width:calc(100vw - 32px); max-height:86vh; display:flex; flex-direction:column; background:var(--bg-elevated); border:1px solid var(--border-strong); box-shadow:0 12px 32px rgba(0,0,0,.35); }
  .custom-modal { width:640px; }
  .modal-body { padding:12px; overflow-y:auto; }
  .custom-body { max-height:70vh; }
  .form-grid { display:grid; grid-template-columns:1fr 1fr; gap:8px; }
  .model-advanced { margin:4px 0; }
  .model-advanced summary { font-size:11px; color:var(--text-dim); cursor:pointer; }
  label { display:block; font-family:var(--font-ui); font-size:12px; color:var(--text-dim); }
  .inline-actions { display:flex; gap:8px; margin:0 0 8px; }
  .advanced { border-top:1px solid var(--border); border-bottom:1px solid var(--border); padding:8px 0; margin-bottom:8px; font-family:var(--font-ui); font-size:12px; color:var(--text-dim); }
  .advanced summary { cursor:pointer; color:var(--text); margin-bottom:8px; }
  textarea { width:100%; min-height:52px; margin:4px 0 8px; box-sizing:border-box; resize:vertical; background:var(--bg-input); border:1px solid var(--border); color:var(--text); font-family:var(--font-mono); font-size:12px; }
  .nested-title { margin-top:12px; }
  .candidate-row, .model-editor { display:grid; grid-template-columns:1fr auto; gap:8px; align-items:center; border-top:1px solid var(--border); padding:6px 0; font-family:var(--font-ui); font-size:12px; color:var(--text); }
  .candidate-row small { display:block; color:var(--text-dim); }
  .model-editor { grid-template-columns:1fr 1fr 90px 90px auto; }
  .model-editor .model-search { margin-bottom:0; }
  .error-text { color:var(--accent); font-family:var(--font-ui); font-size:12px; margin:0 0 8px; }
</style>
