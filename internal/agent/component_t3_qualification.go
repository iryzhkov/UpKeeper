package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Layout detection follows npm's declared entrypoint and the launcher's platform
// resolution. Loading node-pty with Node is meaningful only for the legacy layout.
const t3PackageLayout = `// t3PackageLayout: require package evidence rather than treating MODULE_NOT_FOUND as modern.
const fs=require('node:fs'),p=require('node:path'),m=require('node:module');
try {
 const entry=fs.realpathSync(process.argv[1]),req=m.createRequire(entry);
 const manifest=p.join(p.dirname(p.dirname(entry)),'package.json'),pkg=JSON.parse(fs.readFileSync(manifest,'utf8'));
 const version=process.argv[2];
 if(pkg.name!=='t3'||pkg.version!==version||typeof pkg.bin?.t3!=='string'||fs.realpathSync(p.resolve(p.dirname(manifest),pkg.bin.t3))!==entry) throw 0;
 const legacy=pkg.bin.t3==='./dist/bin.mjs'&&typeof pkg.dependencies?.['node-pty']==='string';
 const key=process.platform+'-'+process.arch,platform='@t3code/t3-'+key;
 const modern=pkg.bin.t3==='./bin/t3.js'&&pkg.optionalDependencies?.[platform]===version&&!pkg.dependencies?.['node-pty'];
 if(legacy&&!pkg.optionalDependencies?.[platform]) { console.log(JSON.stringify({kind:'legacy'})); }
 else if(modern&&!legacy&&['linux-x64','linux-arm64'].includes(key)) {
  const file=req.resolve(platform+'/package.json'),native=JSON.parse(fs.readFileSync(file,'utf8'));
  const exe=p.join(p.dirname(file),'t3'),stat=fs.statSync(exe);
  if(native.name!==platform||native.version!==version||native.os?.length!==1||native.os[0]!==process.platform||
    native.cpu?.length!==1||native.cpu[0]!==process.arch||!stat.isFile()||(stat.mode&0o111)===0) throw 0;
  fs.accessSync(exe,fs.constants.X_OK);
  console.log(JSON.stringify({kind:'executable',executable:fs.realpathSync(exe)}));
 } else throw 0;
} catch { console.error('unsupported, malformed or incomplete T3 package layout'); process.exit(1); }
`

// The executable owns its runtime and native ABI. Qualify that runtime through
// its isolated server and PTY API instead of importing its modules into Node.
// The helper's 20s deadline and cleanup precede the runner's 35s outer bound.
const t3ExecutableQualification = `// Qualified against T3 v0.0.45's token exchange and Effect JSON RPC contracts.
// All logs and private credentials stay inside the helper; output is a fixed stage.
const p=require('node:path'),net=require('node:net');
const {spawn}=require('node:child_process'),{randomBytes}=require('node:crypto');
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
let root=process.argv[2],child,ws,exited=false,stage='isolated startup',closing=false;
async function cleanup() {
 if(closing)return;closing=true;
 if(ws)ws.close();
 // The supervisor kills and reaps the complete tree after this helper exits.
}
process.on('SIGTERM',()=>{if(!closing)cleanup().finally(()=>process.exit(1));});
process.on('SIGINT',()=>{if(!closing)cleanup().finally(()=>process.exit(1));});
(async()=>{
 let deadlineTimer;
 try {
  const reserve=net.createServer();
  await new Promise((r,j)=>{reserve.once('error',j);reserve.listen(0,'127.0.0.1',r);});
  const port=reserve.address().port;
  await new Promise(r=>reserve.close(r));
  const token=randomBytes(32).toString('hex');
  const env={PATH:p.dirname(process.execPath)+':/usr/bin:/bin',HOME:root,
   XDG_CONFIG_HOME:p.join(root,'config'),XDG_DATA_HOME:p.join(root,'data'),
   XDG_CACHE_HOME:p.join(root,'cache'),XDG_STATE_HOME:p.join(root,'state'),
   TMPDIR:root,SHELL:'/bin/sh',ENV:'/dev/null',BASH_ENV:'/dev/null',
   T3CODE_HOME:p.join(root,'t3'),
   T3CODE_AUTO_BOOTSTRAP_PROJECT_FROM_CWD:'false',T3CODE_TAILSCALE_SERVE:'false',
   DO_NOT_TRACK:'1'};
  child=spawn(process.argv[1],['start','--mode','desktop','--host','127.0.0.1','--port',String(port),
   '--base-dir',env.T3CODE_HOME,'--no-browser','--bootstrap-fd','3',root],
   {env,cwd:root,detached:true,stdio:['ignore','ignore','ignore','pipe']});
  child.stdio[3].on('error',()=>{});
  child.stdio[3].end(JSON.stringify({mode:'desktop',noBrowser:true,port,
   t3Home:env.T3CODE_HOME,host:'127.0.0.1',desktopBootstrapToken:token,
   tailscaleServeEnabled:false,tailscaleServePort:443})+'\n');
  child.once('error',()=>{exited=true;});child.once('exit',()=>{exited=true;});
  const qualify=async()=>{
   const base='http://127.0.0.1:'+port;
   const form=new URLSearchParams({grant_type:'urn:ietf:params:oauth:grant-type:token-exchange',
    subject_token:token,subject_token_type:'urn:t3:params:oauth:token-type:environment-bootstrap',
    requested_token_type:'urn:ietf:params:oauth:token-type:access_token',scope:'terminal:operate'});
   let access;
   for(;;) {
    if(exited)throw new Error('startup');
    try {
     const res=await fetch(base+'/oauth/token',{method:'POST',body:form,signal:AbortSignal.timeout(1000)});
     if(res.ok){access=(await res.json()).access_token;break;}
    } catch {}
    await sleep(100);
   }
   if(!access)throw new Error('auth');
   stage='isolated PTY ticket';
   const response=await fetch(base+'/api/auth/websocket-ticket',{method:'POST',
    headers:{Authorization:'Bearer '+access},signal:AbortSignal.timeout(2000)});
   if(!response.ok)throw new Error('ticket');
   const ticket=(await response.json()).ticket;
   stage='isolated PTY websocket';
   ws=new WebSocket('ws://127.0.0.1:'+port+'/ws?wsTicket='+encodeURIComponent(ticket));
   await new Promise((r,j)=>{ws.onopen=r;ws.onerror=j;});
   let id=0;const pending=new Map();
   ws.onmessage=event=>{
    try {
     const decoded=JSON.parse(event.data);
     for(const msg of Array.isArray(decoded)?decoded:[decoded]) {
      const waiter=pending.get(String(msg.requestId));
      if(msg._tag==='Exit'&&waiter) {
       pending.delete(String(msg.requestId));
       if(msg.exit?._tag==='Success')waiter.resolve(msg.exit.value);else waiter.reject(new Error('rpc'));
      }
     }
    } catch {}
   };
   const rpc=(tag,payload)=>new Promise((resolve,reject)=>{
    const requestId=String(++id);pending.set(requestId,{resolve,reject});
    ws.send(JSON.stringify({_tag:'Request',id:requestId,tag,payload,headers:[]}));
   });
   const session={threadId:'upkeeper-qualification',terminalId:'term-1'};
   stage='isolated PTY open';
   const opened=await rpc('terminal.open',{...session,cwd:root,cols:80,rows:24});
   if(opened.status!=='running'||!Number.isInteger(opened.pid)||opened.pid<=0)throw new Error('pty');
   // The marker is split in the input so terminal echo cannot satisfy the check.
   const marker=randomBytes(16).toString('hex');
   stage='isolated PTY output';
   await rpc('terminal.write',{...session,data:"printf '%s%s\\n' '"+marker.slice(0,16)+"' '"+marker.slice(16)+"'\n"});
   let found=false;
   for(let i=0;i<40;i++){
    await sleep(100);
    const snapshot=await rpc('terminal.open',{...session,cwd:root,cols:80,rows:24});
    if(snapshot.history.includes(marker)){found=true;break;}
   }
   if(!found)throw new Error('pty-output');
   await rpc('terminal.close',{...session,deleteHistory:true});
  };
  await Promise.race([qualify(),new Promise((_,j)=>{deadlineTimer=setTimeout(()=>j(new Error('deadline')),20000);})]);
  clearTimeout(deadlineTimer);
  stage='cleanup';await cleanup();
  console.log('isolated startup and PTY qualified');
 } catch {
  clearTimeout(deadlineTimer);
  try{await cleanup();}catch{stage='cleanup';}
  console.error(stage+' qualification failed');process.exitCode=1;
 } finally {
  // Pending RPC/fetch promises must not keep a failed qualification alive.
  process.exit(process.exitCode||0);
 }
})();
`

func t3QualifyInstalled(c *Context, node, t3, version string) error {
	fail := func(reason string) error {
		return Failf("installed T3 CLI failed its native startup check; preserving the running service (%s)", reason)
	}
	layout := c.Run([]string{node, "-e", t3PackageLayout, t3, version}, 15*time.Second, "")
	var pkg struct {
		Kind       string `json:"kind"`
		Executable string `json:"executable"`
	}
	if !layout.OK || json.Unmarshal([]byte(layout.Output), &pkg) != nil ||
		(pkg.Kind != "legacy" && (pkg.Kind != "executable" || !filepath.IsAbs(pkg.Executable))) {
		return fail("package layout")
	}
	// --version is a separate identity gate, never startup proof.
	probe := c.Run([]string{t3, "--version"}, 30*time.Second, "")
	got := strings.TrimSpace(probe.Output)
	if !probe.OK || (got != "t3 v"+version && got != "t3 "+version) {
		return fail("version")
	}
	if pkg.Kind == "legacy" {
		const loadNative = "const fs=require('node:fs'),m=require('node:module');" +
			"m.createRequire(fs.realpathSync(process.argv[1]))('node-pty')"
		if !c.Run([]string{node, "-e", loadNative, t3}, 30*time.Second, "").OK {
			return fail("legacy native ABI")
		}
	} else {
		executableVersion := c.Run([]string{pkg.Executable, "--version"}, 10*time.Second, "")
		if !executableVersion.OK || (strings.TrimSpace(executableVersion.Output) != "t3 v"+version && strings.TrimSpace(executableVersion.Output) != "t3 "+version) {
			return fail("platform executable version")
		}
		supervisor, err := os.Executable()
		if err != nil {
			return fail("qualification supervisor")
		}
		result := c.Run([]string{supervisor, t3QualificationSupervisorArg, node, pkg.Executable}, 35*time.Second, "")
		if !result.OK || result.Output != "isolated startup and PTY qualified; cleanup complete" {
			reason := "isolated startup/PTY"
			for _, stage := range []string{"isolated startup", "isolated PTY ticket", "isolated PTY websocket", "isolated PTY open", "isolated PTY output", "cleanup"} {
				if result.Output == stage+" qualification failed" {
					reason = stage
				}
			}
			return fail(reason)
		}
	}
	return nil
}
