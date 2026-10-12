export namespace desktopclient {

	export class ConnectionInfo {
	    instance_id: string;
	    generation: string;
	    workspace: string;

	    static createFrom(source: any = {}) {
	        return new ConnectionInfo(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.instance_id = source["instance_id"];
	        this.generation = source["generation"];
	        this.workspace = source["workspace"];
	    }
	}
	export class HTTPResponse {
	    status: number;
	    content_type: string;
	    body: string;

	    static createFrom(source: any = {}) {
	        return new HTTPResponse(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.status = source["status"];
	        this.content_type = source["content_type"];
	        this.body = source["body"];
	    }
	}

}
