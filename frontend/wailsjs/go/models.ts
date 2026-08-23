export namespace agent {

	export class BackgroundProcessDisplay {
	    id: string;
	    command: string;
	    reason: string;
	    exitCode: number;
	    output: string;

	    static createFrom(source: any = {}) {
	        return new BackgroundProcessDisplay(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.command = source["command"];
	        this.reason = source["reason"];
	        this.exitCode = source["exitCode"];
	        this.output = source["output"];
	    }
	}
	export class CustomProviderModelInput {
	    id: string;
	    name: string;
	    contextWindow: number;
	    maxOutputTokens: number;
	    inputModalities?: string[];
	    systemRole?: string;
	    usageInStream?: boolean;
	    extraBody?: Record<string, any>;
	    cost?: catalog.Cost;
	    protocolMetadata?: catalog.ProtocolMetadata;
	    hidden?: boolean;

	    static createFrom(source: any = {}) {
	        return new CustomProviderModelInput(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.contextWindow = source["contextWindow"];
	        this.maxOutputTokens = source["maxOutputTokens"];
	        this.inputModalities = source["inputModalities"];
	        this.systemRole = source["systemRole"];
	        this.usageInStream = source["usageInStream"];
	        this.extraBody = source["extraBody"];
	        this.cost = this.convertValues(source["cost"], catalog.Cost);
	        this.protocolMetadata = this.convertValues(source["protocolMetadata"], catalog.ProtocolMetadata);
	        this.hidden = source["hidden"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class CustomProviderRequest {
	    id: string;
	    name: string;
	    baseURL: string;
	    apiKeyEnv: string;
	    apiKey: string;
	    headers?: Record<string, string>;
	    options?: Record<string, any>;
	    systemRole?: string;
	    usageInStream?: boolean;
	    maxTokensField?: string;
	    extraBody?: Record<string, any>;
	    protocolMetadata?: catalog.ProtocolMetadata;
	    hidden?: boolean;
	    discovery: boolean;
	    models: CustomProviderModelInput[];

	    static createFrom(source: any = {}) {
	        return new CustomProviderRequest(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.baseURL = source["baseURL"];
	        this.apiKeyEnv = source["apiKeyEnv"];
	        this.apiKey = source["apiKey"];
	        this.headers = source["headers"];
	        this.options = source["options"];
	        this.systemRole = source["systemRole"];
	        this.usageInStream = source["usageInStream"];
	        this.maxTokensField = source["maxTokensField"];
	        this.extraBody = source["extraBody"];
	        this.protocolMetadata = this.convertValues(source["protocolMetadata"], catalog.ProtocolMetadata);
	        this.hidden = source["hidden"];
	        this.discovery = source["discovery"];
	        this.models = this.convertValues(source["models"], CustomProviderModelInput);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class DiscoveryModelCandidate {
	    id: string;
	    name: string;
	    contextWindow: number;
	    maxOutputTokens: number;
	    cost?: catalog.Cost;
	    usable: boolean;

	    static createFrom(source: any = {}) {
	        return new DiscoveryModelCandidate(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.contextWindow = source["contextWindow"];
	        this.maxOutputTokens = source["maxOutputTokens"];
	        this.cost = this.convertValues(source["cost"], catalog.Cost);
	        this.usable = source["usable"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SubagentSessionLink {
	    index: number;
	    sessionId: string;

	    static createFrom(source: any = {}) {
	        return new SubagentSessionLink(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.index = source["index"];
	        this.sessionId = source["sessionId"];
	    }
	}
	export class DisplayMessage {
	    type: string;
	    content?: string;
	    turn?: number;
	    id?: string;
	    name?: string;
	    args?: string;
	    done?: boolean;
	    success?: boolean;
	    result?: string;
	    metadata?: Record<string, any>;
	    subagentSessionIds?: SubagentSessionLink[];
	    backgroundProcess?: BackgroundProcessDisplay;

	    static createFrom(source: any = {}) {
	        return new DisplayMessage(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.type = source["type"];
	        this.content = source["content"];
	        this.turn = source["turn"];
	        this.id = source["id"];
	        this.name = source["name"];
	        this.args = source["args"];
	        this.done = source["done"];
	        this.success = source["success"];
	        this.result = source["result"];
	        this.metadata = source["metadata"];
	        this.subagentSessionIds = this.convertValues(source["subagentSessionIds"], SubagentSessionLink);
	        this.backgroundProcess = this.convertValues(source["backgroundProcess"], BackgroundProcessDisplay);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class HydrationCursor {
	    committedTurn: number;
	    committedSeq: number;
	    rewriteEpoch: number;

	    static createFrom(source: any = {}) {
	        return new HydrationCursor(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.committedTurn = source["committedTurn"];
	        this.committedSeq = source["committedSeq"];
	        this.rewriteEpoch = source["rewriteEpoch"];
	    }
	}
	export class HydrationRow {
	    seq: number;
	    message: DisplayMessage;

	    static createFrom(source: any = {}) {
	        return new HydrationRow(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.seq = source["seq"];
	        this.message = this.convertValues(source["message"], DisplayMessage);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class PromptWarning {
	    kind: string;
	    message: string;

	    static createFrom(source: any = {}) {
	        return new PromptWarning(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.kind = source["kind"];
	        this.message = source["message"];
	    }
	}
	export class QueuedItem {
	    id: string;
	    content: string;

	    static createFrom(source: any = {}) {
	        return new QueuedItem(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.content = source["content"];
	    }
	}
	export class QueueState {
	    items: QueuedItem[];
	    version: number;

	    static createFrom(source: any = {}) {
	        return new QueueState(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.items = this.convertValues(source["items"], QueuedItem);
	        this.version = source["version"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ModelInfo {
	    ref: string;
	    provider: string;
	    model: string;
	    displayName: string;
	    contextWindow: number;
	    cost?: catalog.Cost;
	    incomplete: boolean;

	    static createFrom(source: any = {}) {
	        return new ModelInfo(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ref = source["ref"];
	        this.provider = source["provider"];
	        this.model = source["model"];
	        this.displayName = source["displayName"];
	        this.contextWindow = source["contextWindow"];
	        this.cost = this.convertValues(source["cost"], catalog.Cost);
	        this.incomplete = source["incomplete"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class TokenEntry {
	    provider: string;
	    model: string;
	    cache: number;
	    input: number;
	    output: number;
	    known: boolean;

	    static createFrom(source: any = {}) {
	        return new TokenEntry(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.provider = source["provider"];
	        this.model = source["model"];
	        this.cache = source["cache"];
	        this.input = source["input"];
	        this.output = source["output"];
	        this.known = source["known"];
	    }
	}
	export class TokenReport {
	    total: TokenEntry;
	    perModel: TokenEntry[];
	    contextUsed: number;
	    contextWindow: number;

	    static createFrom(source: any = {}) {
	        return new TokenReport(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.total = this.convertValues(source["total"], TokenEntry);
	        this.perModel = this.convertValues(source["perModel"], TokenEntry);
	        this.contextUsed = source["contextUsed"];
	        this.contextWindow = source["contextWindow"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SessionSummary {
	    id: string;
	    createdAt: string;
	    lastActivity: number;
	    state: string;
	    archivedAt: number;
	    projectPath: string;
	    parentSessionId?: string;

	    static createFrom(source: any = {}) {
	        return new SessionSummary(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.createdAt = source["createdAt"];
	        this.lastActivity = source["lastActivity"];
	        this.state = source["state"];
	        this.archivedAt = source["archivedAt"];
	        this.projectPath = source["projectPath"];
	        this.parentSessionId = source["parentSessionId"];
	    }
	}
	export class HydrationState {
	    session: SessionSummary;
	    messages: DisplayMessage[];
	    tail: HydrationRow[];
	    errors: HydrationRow[];
	    cursor: HydrationCursor;
	    transcriptReplay: boolean;
	    tokens: TokenReport;
	    model: ModelInfo;
	    busy: boolean;
	    compacting: boolean;
	    queue: QueueState;
	    warnings: PromptWarning[];
	    permissions: permission.Request[];
	    readOnly: boolean;
	    assistantOpen: boolean;

	    static createFrom(source: any = {}) {
	        return new HydrationState(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.session = this.convertValues(source["session"], SessionSummary);
	        this.messages = this.convertValues(source["messages"], DisplayMessage);
	        this.tail = this.convertValues(source["tail"], HydrationRow);
	        this.errors = this.convertValues(source["errors"], HydrationRow);
	        this.cursor = this.convertValues(source["cursor"], HydrationCursor);
	        this.transcriptReplay = source["transcriptReplay"];
	        this.tokens = this.convertValues(source["tokens"], TokenReport);
	        this.model = this.convertValues(source["model"], ModelInfo);
	        this.busy = source["busy"];
	        this.compacting = source["compacting"];
	        this.queue = this.convertValues(source["queue"], QueueState);
	        this.warnings = this.convertValues(source["warnings"], PromptWarning);
	        this.permissions = this.convertValues(source["permissions"], permission.Request);
	        this.readOnly = source["readOnly"];
	        this.assistantOpen = source["assistantOpen"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ModelCompletion {
	    context_window: number;
	    max_output_tokens: number;

	    static createFrom(source: any = {}) {
	        return new ModelCompletion(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.context_window = source["context_window"];
	        this.max_output_tokens = source["max_output_tokens"];
	    }
	}
	export class ModelConfigInput {
	    name: string;
	    contextWindow: number;
	    maxOutputTokens: number;
	    inputModalities?: string[];
	    systemRole?: string;
	    usageInStream?: boolean;
	    extraBody?: Record<string, any>;
	    cost?: catalog.Cost;
	    protocolMetadata?: catalog.ProtocolMetadata;

	    static createFrom(source: any = {}) {
	        return new ModelConfigInput(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.contextWindow = source["contextWindow"];
	        this.maxOutputTokens = source["maxOutputTokens"];
	        this.inputModalities = source["inputModalities"];
	        this.systemRole = source["systemRole"];
	        this.usageInStream = source["usageInStream"];
	        this.extraBody = source["extraBody"];
	        this.cost = this.convertValues(source["cost"], catalog.Cost);
	        this.protocolMetadata = this.convertValues(source["protocolMetadata"], catalog.ProtocolMetadata);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ModelConfigView {
	    id: string;
	    name: string;
	    contextWindow: number;
	    maxOutputTokens: number;
	    inputModalities: string[];
	    systemRole: string;
	    usageInStream: boolean;
	    extraBody: Record<string, any>;
	    cost?: catalog.Cost;
	    protocolMetadata?: catalog.ProtocolMetadata;
	    hidden: boolean;
	    source: string;

	    static createFrom(source: any = {}) {
	        return new ModelConfigView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.contextWindow = source["contextWindow"];
	        this.maxOutputTokens = source["maxOutputTokens"];
	        this.inputModalities = source["inputModalities"];
	        this.systemRole = source["systemRole"];
	        this.usageInStream = source["usageInStream"];
	        this.extraBody = source["extraBody"];
	        this.cost = this.convertValues(source["cost"], catalog.Cost);
	        this.protocolMetadata = this.convertValues(source["protocolMetadata"], catalog.ProtocolMetadata);
	        this.hidden = source["hidden"];
	        this.source = source["source"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

	export class ModelListEntry {
	    ref: string;
	    provider: string;
	    providerName: string;
	    model: string;
	    displayName: string;
	    contextWindow: number;
	    maxOutputTokens: number;
	    cost?: catalog.Cost;
	    hidden: boolean;
	    providerHidden: boolean;
	    incomplete: boolean;
	    default: boolean;
	    source: string;

	    static createFrom(source: any = {}) {
	        return new ModelListEntry(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ref = source["ref"];
	        this.provider = source["provider"];
	        this.providerName = source["providerName"];
	        this.model = source["model"];
	        this.displayName = source["displayName"];
	        this.contextWindow = source["contextWindow"];
	        this.maxOutputTokens = source["maxOutputTokens"];
	        this.cost = this.convertValues(source["cost"], catalog.Cost);
	        this.hidden = source["hidden"];
	        this.providerHidden = source["providerHidden"];
	        this.incomplete = source["incomplete"];
	        this.default = source["default"];
	        this.source = source["source"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ProjectSummary {
	    id: string;
	    name: string;
	    path: string;
	    createdAt: string;
	    lastActivity: number;

	    static createFrom(source: any = {}) {
	        return new ProjectSummary(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.path = source["path"];
	        this.createdAt = source["createdAt"];
	        this.lastActivity = source["lastActivity"];
	    }
	}

	export class ProviderConfigInput {
	    name: string;
	    baseURL: string;
	    apiKeyEnv: string;
	    headers?: Record<string, string>;
	    options?: Record<string, any>;
	    systemRole?: string;
	    usageInStream?: boolean;
	    maxTokensField?: string;
	    extraBody?: Record<string, any>;
	    discovery?: boolean;
	    protocolMetadata?: catalog.ProtocolMetadata;

	    static createFrom(source: any = {}) {
	        return new ProviderConfigInput(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.baseURL = source["baseURL"];
	        this.apiKeyEnv = source["apiKeyEnv"];
	        this.headers = source["headers"];
	        this.options = source["options"];
	        this.systemRole = source["systemRole"];
	        this.usageInStream = source["usageInStream"];
	        this.maxTokensField = source["maxTokensField"];
	        this.extraBody = source["extraBody"];
	        this.discovery = source["discovery"];
	        this.protocolMetadata = this.convertValues(source["protocolMetadata"], catalog.ProtocolMetadata);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ProviderConfigView {
	    id: string;
	    name: string;
	    builtin: boolean;
	    connected: boolean;
	    baseURL: string;
	    apiKeyEnv: string;
	    generatedKeyEnv: string;
	    headers: Record<string, string>;
	    userHeaders: Record<string, string>;
	    options: Record<string, any>;
	    systemRole: string;
	    usageInStream: boolean;
	    maxTokensField: string;
	    extraBody: Record<string, any>;
	    discovery: boolean;
	    protocolMetadata?: catalog.ProtocolMetadata;
	    models: ModelConfigView[];

	    static createFrom(source: any = {}) {
	        return new ProviderConfigView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.builtin = source["builtin"];
	        this.connected = source["connected"];
	        this.baseURL = source["baseURL"];
	        this.apiKeyEnv = source["apiKeyEnv"];
	        this.generatedKeyEnv = source["generatedKeyEnv"];
	        this.headers = source["headers"];
	        this.userHeaders = source["userHeaders"];
	        this.options = source["options"];
	        this.systemRole = source["systemRole"];
	        this.usageInStream = source["usageInStream"];
	        this.maxTokensField = source["maxTokensField"];
	        this.extraBody = source["extraBody"];
	        this.discovery = source["discovery"];
	        this.protocolMetadata = this.convertValues(source["protocolMetadata"], catalog.ProtocolMetadata);
	        this.models = this.convertValues(source["models"], ModelConfigView);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ProviderStatus {
	    id: string;
	    name: string;
	    builtin: boolean;
	    connected: boolean;
	    keySource: string;
	    disconnectable: boolean;
	    removable: boolean;
	    apiKeyEnv: string;
	    generatedKeyEnv?: string;
	    baseURL: string;
	    discovery: boolean;
	    modelCount: number;
	    usableModels: number;

	    static createFrom(source: any = {}) {
	        return new ProviderStatus(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.builtin = source["builtin"];
	        this.connected = source["connected"];
	        this.keySource = source["keySource"];
	        this.disconnectable = source["disconnectable"];
	        this.removable = source["removable"];
	        this.apiKeyEnv = source["apiKeyEnv"];
	        this.generatedKeyEnv = source["generatedKeyEnv"];
	        this.baseURL = source["baseURL"];
	        this.discovery = source["discovery"];
	        this.modelCount = source["modelCount"];
	        this.usableModels = source["usableModels"];
	    }
	}


	export class RuntimeCompactionConfig {
	    threshold_pct: number;

	    static createFrom(source: any = {}) {
	        return new RuntimeCompactionConfig(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.threshold_pct = source["threshold_pct"];
	    }
	}
	export class RuntimeToolsConfig {
	    max_output_bytes: number;
	    read_max_lines: number;
	    read_line_max_chars: number;
	    command_timeout: number;
	    max_background_processes: number;

	    static createFrom(source: any = {}) {
	        return new RuntimeToolsConfig(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.max_output_bytes = source["max_output_bytes"];
	        this.read_max_lines = source["read_max_lines"];
	        this.read_line_max_chars = source["read_line_max_chars"];
	        this.command_timeout = source["command_timeout"];
	        this.max_background_processes = source["max_background_processes"];
	    }
	}
	export class RuntimeSubagentsConfig {
	    max_concurrent: number;

	    static createFrom(source: any = {}) {
	        return new RuntimeSubagentsConfig(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.max_concurrent = source["max_concurrent"];
	    }
	}
	export class RuntimeSessionsConfig {
	    archive_after_days: number;
	    delete_after_archive_days: number;

	    static createFrom(source: any = {}) {
	        return new RuntimeSessionsConfig(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.archive_after_days = source["archive_after_days"];
	        this.delete_after_archive_days = source["delete_after_archive_days"];
	    }
	}
	export class RuntimeConfigSettings {
	    sessions: RuntimeSessionsConfig;
	    compaction: RuntimeCompactionConfig;
	    subagents: RuntimeSubagentsConfig;
	    tools: RuntimeToolsConfig;

	    static createFrom(source: any = {}) {
	        return new RuntimeConfigSettings(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.sessions = this.convertValues(source["sessions"], RuntimeSessionsConfig);
	        this.compaction = this.convertValues(source["compaction"], RuntimeCompactionConfig);
	        this.subagents = this.convertValues(source["subagents"], RuntimeSubagentsConfig);
	        this.tools = this.convertValues(source["tools"], RuntimeToolsConfig);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}




	export class SnapshotFile {
	    path: string;
	    existed: boolean;

	    static createFrom(source: any = {}) {
	        return new SnapshotFile(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.existed = source["existed"];
	    }
	}
	export class Snapshot {
	    turn: number;
	    files: SnapshotFile[];

	    static createFrom(source: any = {}) {
	        return new Snapshot(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.turn = source["turn"];
	        this.files = this.convertValues(source["files"], SnapshotFile);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}


	export class SubmitResult {
	    started: boolean;
	    turn?: number;
	    queue: QueuedItem[];
	    version: number;

	    static createFrom(source: any = {}) {
	        return new SubmitResult(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.started = source["started"];
	        this.turn = source["turn"];
	        this.queue = this.convertValues(source["queue"], QueuedItem);
	        this.version = source["version"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}


	export class TurnActionResult {
	    action: string;
	    turn: number;
	    targetTurn: number;
	    sessionChanged: boolean;
	    prefill?: string;
	    warning?: string;
	    restoredFiles?: string[];
	    skippedFiles?: snapshot.SkippedRevert[];
	    session: SessionSummary;
	    messages?: DisplayMessage[];
	    tokens: TokenReport;

	    static createFrom(source: any = {}) {
	        return new TurnActionResult(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.action = source["action"];
	        this.turn = source["turn"];
	        this.targetTurn = source["targetTurn"];
	        this.sessionChanged = source["sessionChanged"];
	        this.prefill = source["prefill"];
	        this.warning = source["warning"];
	        this.restoredFiles = source["restoredFiles"];
	        this.skippedFiles = this.convertValues(source["skippedFiles"], snapshot.SkippedRevert);
	        this.session = this.convertValues(source["session"], SessionSummary);
	        this.messages = this.convertValues(source["messages"], DisplayMessage);
	        this.tokens = this.convertValues(source["tokens"], TokenReport);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace catalog {

	export class Cost {
	    input?: number;
	    output?: number;
	    cache_read?: number;
	    cache_write?: number;

	    static createFrom(source: any = {}) {
	        return new Cost(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.input = source["input"];
	        this.output = source["output"];
	        this.cache_read = source["cache_read"];
	        this.cache_write = source["cache_write"];
	    }
	}
	export class ProtocolMetadata {
	    family?: string;
	    must_preserve?: string[];
	    drop?: string[];

	    static createFrom(source: any = {}) {
	        return new ProtocolMetadata(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.family = source["family"];
	        this.must_preserve = source["must_preserve"];
	        this.drop = source["drop"];
	    }
	}

}

export namespace main {

	export class CurrentModelResult {
	    model: agent.ModelInfo;
	    superseded: boolean;

	    static createFrom(source: any = {}) {
	        return new CurrentModelResult(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.model = this.convertValues(source["model"], agent.ModelInfo);
	        this.superseded = source["superseded"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace permission {

	export class Request {
	    id: string;
	    session_id?: string;
	    project_id?: string;
	    tool: string;
	    args: string;
	    resolved_arg?: string;
	    can_allow_all?: boolean;
	    disable_project_save?: boolean;
	    batch_index?: number;
	    batch_total?: number;
	    batch_files?: string[];
	    batch_resolved_files?: string[];

	    static createFrom(source: any = {}) {
	        return new Request(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.session_id = source["session_id"];
	        this.project_id = source["project_id"];
	        this.tool = source["tool"];
	        this.args = source["args"];
	        this.resolved_arg = source["resolved_arg"];
	        this.can_allow_all = source["can_allow_all"];
	        this.disable_project_save = source["disable_project_save"];
	        this.batch_index = source["batch_index"];
	        this.batch_total = source["batch_total"];
	        this.batch_files = source["batch_files"];
	        this.batch_resolved_files = source["batch_resolved_files"];
	    }
	}
	export class Suggestion {
	    rule: string;
	    label: string;

	    static createFrom(source: any = {}) {
	        return new Suggestion(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.rule = source["rule"];
	        this.label = source["label"];
	    }
	}

}

export namespace snapshot {

	export class SkippedRevert {
	    path: string;
	    reason: string;

	    static createFrom(source: any = {}) {
	        return new SkippedRevert(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.reason = source["reason"];
	    }
	}
	export class RevertResult {
	    restored?: string[];
	    skipped?: SkippedRevert[];

	    static createFrom(source: any = {}) {
	        return new RevertResult(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.restored = source["restored"];
	        this.skipped = this.convertValues(source["skipped"], SkippedRevert);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}
