export namespace agent {
	
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
	export class ModelListEntry {
	    ref: string;
	    provider: string;
	    providerName: string;
	    model: string;
	    displayName: string;
	    contextWindow: number;
	    cost?: catalog.Cost;
	    hidden: boolean;
	    providerHidden: boolean;
	    incomplete: boolean;
	
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
	        this.cost = this.convertValues(source["cost"], catalog.Cost);
	        this.hidden = source["hidden"];
	        this.providerHidden = source["providerHidden"];
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
	export class QueuedMessageTurn {
	    content: string;
	    turn: number;
	
	    static createFrom(source: any = {}) {
	        return new QueuedMessageTurn(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.content = source["content"];
	        this.turn = source["turn"];
	    }
	}
	export class QueuedMessagesResult {
	    appended?: QueuedMessageTurn[];
	    started: QueuedMessageTurn;
	
	    static createFrom(source: any = {}) {
	        return new QueuedMessagesResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.appended = this.convertValues(source["appended"], QueuedMessageTurn);
	        this.started = this.convertValues(source["started"], QueuedMessageTurn);
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
	export class TurnActionResult {
	    action: string;
	    turn: number;
	    targetTurn: number;
	    sessionChanged: boolean;
	    prefill?: string;
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

}

export namespace permission {
	
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

