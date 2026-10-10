package rollout

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// ConfigChange contains reviewed coordinator configuration bytes, never credentials.
type ConfigChange struct {
	BeforeSHA256 string
	AfterSHA256  string
	Content      string
	// Each {config} argument is replaced by the staged configuration path.
	ValidateCommand []string
}

func (p ConfigChange) Validate() error {
	hex := regexp.MustCompile("^[0-9a-f]{64}$")
	if !hex.MatchString(p.BeforeSHA256) || !hex.MatchString(p.AfterSHA256) || p.BeforeSHA256 == p.AfterSHA256 || digest([]byte(p.Content)) != p.AfterSHA256 {
		return errors.New("configuration patch requires distinct reviewed digests and exact after bytes")
	}
	found := false
	for _, arg := range p.ValidateCommand {
		if strings.Contains(arg, "{config}") {
			found = true
		}
	}
	if !validArgv(p.ValidateCommand) || !found {
		return errors.New("configuration patch requires staged-file validator")
	}
	return nil
}

// A completion checkpoint failure keeps the prior unfinished receipt in memory
// as well as on reload; it cannot authorize a retry in the current process.
func transactionRecord(s *State, key string, value any) error {
	old, exists := s.Extra[key]
	err := record(s, key, value)
	if err != nil && exists {
		s.Extra[key] = old
	}
	return err
}

type configReceipt struct {
	Before, After  string
	IntentAt       time.Time
	Done, Restored bool
}

func (b *ProductionBackend) configPatch(ctx context.Context, c Config, s *State) error {
	p := c.CoordinatorConfig
	if p == nil {
		return errors.New("reviewed coordinator configuration transaction required")
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if s.Save == nil {
		return errors.New("configuration intent requires durable journal")
	}
	now := b.Now()
	age := now.Sub(s.BackupCreated)
	if s.BackupSnapshot == "" || s.BackupScratch == "" || s.BackupCreated.IsZero() || age < 0 || age > time.Duration(c.BackupMaxAgeSeconds)*time.Second {
		return errors.New("configuration transaction requires verified fresh backup and scratch restore")
	}
	var backup map[string]string
	if err := evidence(s, "backup", &backup); err != nil {
		return err
	}
	if backup["restore_integrity"] != "ok" || backup["restore_fk_violations"] != "0" || backup["snapshot"] != s.BackupSnapshot || backup["scratch"] != s.BackupScratch {
		return errors.New("configuration transaction requires matching verified integrity and foreign-key backup evidence")
	}
	var r configReceipt
	if _, ok := s.Extra["config-intent"]; ok {
		if err := evidence(s, "config-intent", &r); err != nil {
			return err
		}
		if r.Before != p.BeforeSHA256 || r.After != p.AfterSHA256 || r.IntentAt.IsZero() || r.IntentAt.After(now) || r.Restored {
			return errors.New("configuration receipt differs or is invalid")
		}
		if !r.Done {
			return errors.New("unfinished configuration effect: reconcile by configuration restore; no retry")
		}
		_, err := b.configCommand(ctx, c, "verify", p)
		return err
	}
	r = configReceipt{Before: p.BeforeSHA256, After: p.AfterSHA256, IntentAt: now}
	if err := transactionRecord(s, "config-intent", r); err != nil {
		return err
	}
	if err := freshBackupAt(c, s, b.Now()); err != nil {
		return err
	}
	if _, err := b.configCommand(ctx, c, "install", p); err != nil {
		return err
	}
	r.Done = true
	return transactionRecord(s, "config-intent", r)
}

// configRestore is recovery only. Call before dispatching binary rollback.
func (b *ProductionBackend) configRestore(ctx context.Context, c Config, s *State) error {
	if c.CoordinatorConfig == nil {
		return nil
	}
	if _, ok := s.Extra["config-intent"]; !ok {
		// Absence cannot prove that installation never took effect. The frozen
		// configuration requires its original durable receipt before recovery.
		return errors.New("missing configuration intent: original configuration restoration is unknown")
	}
	if s.Save == nil {
		return errors.New("configuration restore requires durable journal")
	}
	var r configReceipt
	if err := evidence(s, "config-intent", &r); err != nil {
		return err
	}
	p := c.CoordinatorConfig
	if err := p.Validate(); err != nil {
		return err
	}
	if r.Before != p.BeforeSHA256 || r.After != p.AfterSHA256 || r.IntentAt.IsZero() || r.IntentAt.After(b.Now()) {
		return errors.New("configuration restore evidence mismatch")
	}
	if err := transactionRecord(s, "config-restore-intent", r); err != nil {
		return err
	}
	if _, err := b.configCommand(ctx, c, "restore", p); err != nil {
		return err
	}
	if err := b.configBefore(ctx, c); err != nil {
		return fmt.Errorf("verify original configuration after restore: %w", err)
	}
	r.Restored = true
	return transactionRecord(s, "config-intent", r)
}

// configBefore runs immediately before backup can stop or restart the old
// coordinator; it cannot load patched or stale bytes into the previous binary.
func (b *ProductionBackend) configBefore(ctx context.Context, c Config) error {
	if c.CoordinatorConfig == nil {
		return errors.New("reviewed coordinator configuration required")
	}
	if err := c.CoordinatorConfig.Validate(); err != nil {
		return err
	}
	_, err := b.configCommand(ctx, c, "before", c.CoordinatorConfig)
	return err
}

func (b *ProductionBackend) configCommand(ctx context.Context, c Config, mode string, p *ConfigChange) ([]byte, error) {
	validator, err := json.Marshal(p.ValidateCommand)
	if err != nil {
		return nil, err
	}
	return b.on(ctx, c, c.CoordinatorHost, configTransactionScript, mode, p.BeforeSHA256, p.AfterSHA256, base64.StdEncoding.EncodeToString([]byte(p.Content)), string(validator))
}

// Python's file operations are checked individually; subprocess failure never
// becomes a valid receipt. Backup creation is exclusive and survives retries.
const configTransactionScript = `set -euo pipefail
exec python3 - "$@" <<'PY'
import base64,hashlib,json,os,stat,subprocess,sys,tempfile
mode,before,after,encoded,validator=sys.argv[1:]
path=os.path.join(os.environ["HOME"],".config/t3-steward/config.yaml")
backup=path+".bak-rc122-pre"
def regular(p):
 s=os.lstat(p)
 if not stat.S_ISREG(s.st_mode) or s.st_uid != os.getuid(): raise RuntimeError("unsafe configuration ownership/type")
 return s
def read(p):
 regular(p)
 with open(p,"rb") as f: return f.read()
def sha(data): return hashlib.sha256(data).hexdigest()
def syncdir():
 fd=os.open(os.path.dirname(path),os.O_RDONLY|os.O_DIRECTORY)
 try: os.fsync(fd)
 finally: os.close(fd)
current=read(path); meta=regular(path)
live=sha(current)
if mode=="before":
 if live!=before: raise RuntimeError("coordinator backup must use reviewed before configuration")
 print("configuration_before=ok")
 sys.exit(0)
if live not in (before,after) and mode!="restore": raise RuntimeError("unreviewed live configuration")
if os.path.lexists(backup):
 saved=read(backup); original=regular(backup)
 if sha(saved)!=before: raise RuntimeError("original backup digest mismatch")
else:
 if mode!="install" or live!=before: raise RuntimeError("original backup absent")
 fd=os.open(backup,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,stat.S_IMODE(meta.st_mode))
 with os.fdopen(fd,"wb") as f:
  os.fchown(f.fileno(),meta.st_uid,meta.st_gid); os.fchmod(f.fileno(),stat.S_IMODE(meta.st_mode))
  f.write(current); f.flush(); os.fsync(f.fileno())
 syncdir()
 saved=read(backup); original=regular(backup)
 if sha(saved)!=before: raise RuntimeError("new original backup digest mismatch")
if (meta.st_uid,meta.st_gid,stat.S_IMODE(meta.st_mode))!=(original.st_uid,original.st_gid,stat.S_IMODE(original.st_mode)):
 raise RuntimeError("configuration metadata changed")
if mode=="verify":
 if live!=after: raise RuntimeError("configuration is not installed")
elif mode in ("install","restore"):
 target=base64.b64decode(encoded,validate=True) if mode=="install" else saved
 want=after if mode=="install" else before
 if sha(target)!=want: raise RuntimeError("staged content digest mismatch")
 if live!=want:
  fd,tmp=tempfile.mkstemp(prefix=".rc122-config-",dir=os.path.dirname(path))
  try:
   with os.fdopen(fd,"wb") as f:
    os.fchown(f.fileno(),original.st_uid,original.st_gid); os.fchmod(f.fileno(),stat.S_IMODE(original.st_mode))
    f.write(target); f.flush(); os.fsync(f.fileno())
   if mode=="install":
    os.chmod(tmp,0o600)
    command=[arg.replace("{config}",tmp) for arg in json.loads(validator)]
    def validation_facts(s): return (s.st_dev,s.st_ino,s.st_nlink,s.st_uid,s.st_gid,stat.S_IMODE(s.st_mode),s.st_size,s.st_mtime_ns,s.st_ctime_ns)
    staged_facts=validation_facts(regular(tmp))
    result=subprocess.run(command,check=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
    if len(result.stdout)>4096: raise RuntimeError("oversized validation receipt")
    def unique(pairs):
     d={}
     for k,v in pairs:
      if k in d: raise RuntimeError("duplicate validation field")
      d[k]=v
     return d
    receipt=json.loads(result.stdout,object_pairs_hook=unique)
    expected={"schema_version":1,"kind":"config-validation","valid":True,"file_sha256":want}
    if receipt!=expected or type(receipt.get("schema_version")) is not int or receipt.get("valid") is not True: raise RuntimeError("invalid validation receipt")
    if sha(read(tmp))!=want or validation_facts(regular(tmp))!=staged_facts or stat.S_IMODE(regular(tmp).st_mode)!=0o600: raise RuntimeError("validator changed staged file")
    os.chmod(tmp,stat.S_IMODE(original.st_mode))
    with open(tmp,"rb") as staged: os.fsync(staged.fileno())
   if sha(read(path))!=live: raise RuntimeError("concurrent configuration change")
   os.replace(tmp,path); syncdir()
   if sha(read(path))!=want: raise RuntimeError("installed configuration digest mismatch")
  finally:
   if os.path.exists(tmp): os.unlink(tmp)
else: raise RuntimeError("unknown configuration transaction")
print("configuration_"+mode+"=ok")
PY
`

// HaikuCheckCommand is a reviewed adapter until the exact worker advertisement
// API schema is pinned. Host and epoch are supplied as final command arguments.
func (b *ProductionBackend) haikuAdvertisement(ctx context.Context, c Config, host, epoch string) error {
	if len(c.HaikuCheckCommand) == 0 {
		return errors.New("verified Haiku advertisement adapter required")
	}
	args := append(append([]string{}, c.HaikuCheckCommand[1:]...), host, epoch)
	raw, err := b.command(ctx, c, "", c.HaikuCheckCommand[0], args...)
	if err != nil {
		return err
	}
	var receipt struct {
		WorkerID string `json:"worker_id"`
		Epoch    string `json:"coordinator_epoch"`
		Model    string `json:"model"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&receipt); err != nil {
		return err
	}
	if err = dec.Decode(new(any)); err != io.EOF {
		return errors.New("trailing Haiku advertisement JSON")
	}
	if receipt.WorkerID != host || receipt.Epoch != epoch || receipt.Model != "claudeAgent/claude-haiku-5-5" {
		return errors.New("Haiku advertisement identity/epoch/model mismatch")
	}
	return nil
}

type dropinReceipt struct {
	Host, SHA256           string
	Mode, UID, GID         int
	Absent, Done, Restored bool
	IntentAt               time.Time
}

func (b *ProductionBackend) dropinRepair(ctx context.Context, c Config, host string, s *State) (result error) {
	if host != "agent-b" && host != "agent-a" {
		return errors.New("drop-in repair limited to agent-b then agent-a")
	}
	if s.Save == nil {
		return errors.New("drop-in repair requires durable journal")
	}
	if host == "agent-a" {
		var prior dropinReceipt
		if err := evidence(s, "dropin:agent-b", &prior); err != nil || !prior.Done || prior.Restored {
			return errors.New("agent-b drop-in gate must complete first")
		}
	}
	if _, ok := s.Extra["dropin:"+host]; ok {
		return errors.New("existing drop-in attempt requires reconciliation; no retry")
	}
	if len(c.DropinTestCommand) == 0 {
		return errors.New("reviewed fresh-checkout test command required")
	}
	id, err := b.identity(ctx, c)
	if err != nil {
		return err
	}
	if id.Health != "healthy" {
		return errors.New("coordinator unhealthy")
	}
	if err = b.haikuAdvertisement(ctx, c, host, id.Epoch.String()); err != nil {
		return err
	}
	ws, err := b.workers(ctx, c)
	if err != nil {
		return err
	}
	if err = drains(ws, c); err != nil {
		return err
	}
	idle := false
	for _, w := range ws {
		if w.Requirement.WorkerID == host && w.Snapshot.Inventory.Telemetry.RunningAttempts != nil && *w.Snapshot.Inventory.Telemetry.RunningAttempts == 0 && !w.Requirement.Draining && w.Health == "ready" {
			idle = true
		}
	}
	if !idle {
		return errors.New("drop-in worker must be known ready and idle")
	}
	raw, err := b.on(ctx, c, host, dropinScript, "inspect")
	if err != nil {
		return err
	}
	var r dropinReceipt
	if err = json.Unmarshal(raw, &r); err != nil {
		return err
	}
	if !r.Absent && (r.Mode <= 0 || r.Mode > 0777 || r.UID < 0 || r.GID < 0 || !regexp.MustCompile("^[0-9a-f]{64}$").MatchString(r.SHA256)) {
		return errors.New("incomplete original drop-in metadata")
	}
	r.Host = host
	r.IntentAt = b.Now()
	if r.Absent && !contains(c.DropinAbsentHosts, host) {
		return errors.New("absent drop-in requires explicit reviewed rc121 state")
	}
	if err = transactionRecord(s, "dropin:"+host, r); err != nil {
		return err
	}
	defer func() {
		if result != nil && !r.Absent {
			recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
			defer cancel()
			result = errors.Join(result, b.dropinRestore(recovery, c, host, s, id.Epoch.String()))
		}
	}()
	if !r.Absent {
		if _, err = b.on(ctx, c, host, dropinScript, "remove", r.SHA256, fmt.Sprint(r.Mode), fmt.Sprint(r.UID), fmt.Sprint(r.GID)); err != nil {
			return err
		}
	}
	gate := func() error {
		if e := b.waitWorker(ctx, c, host, id.Epoch.String(), c.Version); e != nil {
			return e
		}
		_, e := b.on(ctx, c, host, "set -euo pipefail\nexec "+strings.Join(quoted(c.DropinTestCommand), " "))
		return e
	}
	if err = gate(); err != nil {
		return err
	}
	ws, err = b.workers(ctx, c)
	if err != nil {
		return err
	}
	if err = drains(ws, c); err != nil {
		return err
	}
	final, err := b.identity(ctx, c)
	if err != nil {
		return err
	}
	if final.Health != "healthy" || final.Epoch != id.Epoch {
		return errors.New("coordinator identity/epoch changed during drop-in gate")
	}
	if err = b.haikuAdvertisement(ctx, c, host, id.Epoch.String()); err != nil {
		return err
	}
	r.Done = true
	return transactionRecord(s, "dropin:"+host, r)
}

// dropinRestore never completes enrollment or authorizes advancing to another host.
func (b *ProductionBackend) dropinRestore(ctx context.Context, c Config, host string, s *State, epoch string) error {
	var r dropinReceipt
	if err := evidence(s, "dropin:"+host, &r); err != nil {
		return err
	}
	if r.Absent {
		return errors.New("absent original cannot be restored")
	}
	if r.Host != host || r.IntentAt.IsZero() || r.IntentAt.After(b.Now()) || !regexp.MustCompile("^[0-9a-f]{64}$").MatchString(r.SHA256) {
		return errors.New("drop-in recovery receipt invalid")
	}
	if err := transactionRecord(s, "dropin-restore-intent:"+host, r); err != nil {
		return err
	}
	if _, err := b.on(ctx, c, host, dropinScript, "restore", r.SHA256, fmt.Sprint(r.Mode), fmt.Sprint(r.UID), fmt.Sprint(r.GID)); err != nil {
		return err
	}
	if err := b.waitWorker(ctx, c, host, epoch, c.Version); err != nil {
		return err
	}
	id, err := b.identity(ctx, c)
	if err != nil {
		return err
	}
	if id.Health != "healthy" || id.Epoch.String() != epoch {
		return errors.New("coordinator changed during drop-in recovery")
	}
	ws, err := b.workers(ctx, c)
	if err != nil {
		return err
	}
	if err = drains(ws, c); err != nil {
		return err
	}
	r.Restored = true
	r.Done = false
	return transactionRecord(s, "dropin:"+host, r)
}

const dropinScript = `set -euo pipefail
exec python3 - "$@" <<'PY'
import hashlib,json,os,stat,subprocess,sys
mode=sys.argv[1]
path=os.path.join(os.environ["HOME"],".config/systemd/user/t3-steward-worker.service.d/tmpdir-resolved.conf")
directory=os.path.join(os.environ["HOME"],".local/state/t3-steward-rollout-backups")
backup=os.path.join(directory,"rc122-tmpdir-dropin.conf")
def read(p):
 s=os.lstat(p)
 if not stat.S_ISREG(s.st_mode) or s.st_uid!=os.getuid(): raise RuntimeError("unsafe drop-in")
 with open(p,"rb") as f: return f.read(),s
def sha(p): return hashlib.sha256(p).hexdigest()
def syncdir(p):
 fd=os.open(p,os.O_RDONLY|os.O_DIRECTORY)
 try: os.fsync(fd)
 finally: os.close(fd)
if mode=="inspect":
 if not os.path.lexists(path):
  if os.path.lexists(backup): raise RuntimeError("missing original with existing backup: reconcile prior effect")
  print(json.dumps({"Absent":True}))
 else:
  data,s=read(path)
  print(json.dumps({"SHA256":sha(data),"Mode":stat.S_IMODE(s.st_mode),"UID":s.st_uid,"GID":s.st_gid}))
 sys.exit(0)
want=sys.argv[2]
if mode=="remove":
 data,s=read(path)
 if sha(data)!=want: raise RuntimeError("drop-in changed")
 expected=tuple(int(x) for x in sys.argv[3:6])
 if len(expected)!=3 or (stat.S_IMODE(s.st_mode),s.st_uid,s.st_gid)!=expected: raise RuntimeError("original metadata changed")
 os.makedirs(directory,mode=0o700,exist_ok=True)
 ds=os.lstat(directory)
 if not stat.S_ISDIR(ds.st_mode) or ds.st_uid!=os.getuid() or stat.S_IMODE(ds.st_mode)!=0o700: raise RuntimeError("unsafe backup directory")
 if os.path.lexists(backup):
  saved,bs=read(backup)
  if sha(saved)!=want or (bs.st_uid,bs.st_gid,stat.S_IMODE(bs.st_mode))!=(s.st_uid,s.st_gid,stat.S_IMODE(s.st_mode)): raise RuntimeError("backup mismatch")
 else:
  fd=os.open(backup,os.O_CREAT|os.O_EXCL|os.O_WRONLY|os.O_NOFOLLOW,stat.S_IMODE(s.st_mode))
  with os.fdopen(fd,"wb") as f:
   os.fchown(f.fileno(),s.st_uid,s.st_gid); os.fchmod(f.fileno(),stat.S_IMODE(s.st_mode)); f.write(data); f.flush(); os.fsync(f.fileno())
  syncdir(directory)
  syncdir(os.path.dirname(directory))
 if sha(read(path)[0])!=want: raise RuntimeError("concurrent drop-in change")
 os.unlink(path); syncdir(os.path.dirname(path))
elif mode=="restore":
 data,s=read(backup)
 if sha(data)!=want: raise RuntimeError("backup changed")
 expected=tuple(int(x) for x in sys.argv[3:6])
 if len(expected)!=3 or (stat.S_IMODE(s.st_mode),s.st_uid,s.st_gid)!=expected: raise RuntimeError("backup metadata changed")
 if os.path.lexists(path):
  restored,rs=read(path)
  if sha(restored)!=want or (stat.S_IMODE(rs.st_mode),rs.st_uid,rs.st_gid)!=expected: raise RuntimeError("conflicting original")
 else:
  fd=os.open(path,os.O_CREAT|os.O_EXCL|os.O_WRONLY|os.O_NOFOLLOW,stat.S_IMODE(s.st_mode))
  with os.fdopen(fd,"wb") as f:
   os.fchown(f.fileno(),s.st_uid,s.st_gid); os.fchmod(f.fileno(),stat.S_IMODE(s.st_mode)); f.write(data); f.flush(); os.fsync(f.fileno())
  syncdir(os.path.dirname(path))
else: raise RuntimeError("unknown drop-in operation")
if mode=="restore":
 restored,rs=read(path)
 if sha(restored)!=want or (stat.S_IMODE(rs.st_mode),rs.st_uid,rs.st_gid)!=expected: raise RuntimeError("restored drop-in differs")
subprocess.run(["systemctl","--user","daemon-reload"],check=True)
subprocess.run(["systemctl","--user","restart","t3-steward-worker.service"],check=True)
print("dropin_"+mode+"=ok")
PY
`
