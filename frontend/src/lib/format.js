export function fmtTokens(n, known) {
    if (!known) return '-';
    if (n < 1000) return String(n);
    if (n < 1_000_000) return (n/1000).toFixed(1).replace(/\.0$/, '') + 'k';
    if (n < 1_000_000_000) return (n/1_000_000).toFixed(1).replace(/\.0$/, '') + 'M';
    return (n/1_000_000_000).toFixed(1).replace(/\.0$/, '') + 'B';
}

export function groupByProvider(list) {
    const grouped = [];
    const byProvider = new Map();
    for (const entry of list) {
        const provider = entry.provider || '';
        if (!byProvider.has(provider)) {
            const group = { provider, providerName: entry.providerName || provider, providerHidden: entry.providerHidden || false, models: [] };
            byProvider.set(provider, group);
            grouped.push(group);
        }
        byProvider.get(provider).models.push(entry);
    }
    return grouped;
}
