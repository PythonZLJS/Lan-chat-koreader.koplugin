"""Build only the drag-and-drop KOReader plugin; no app installer or Illusion ZIP."""
from pathlib import Path
import argparse
import hashlib
import json
import os
import shutil
import stat
import struct
import subprocess
import zipfile

ROOT=Path(__file__).resolve().parents[1]
DIST=ROOT/'dist'
parser=argparse.ArgumentParser()
parser.add_argument('--scan',action='store_true')
parser.add_argument('--skip-tests',action='store_true',help='Compile and package without running Go vet/regression tests')
args=parser.parse_args()
compiler=ROOT/'build/go-tools/go1.27.4/go/bin/go.exe'
go=str(compiler) if compiler.is_file() else shutil.which('go')
if not go: raise SystemExit('Building is optional. To build from source, install Go 1.27.4 first.')
zig_local=ROOT/'build/zig-tools/zig-x86_64-windows-0.17.1/zig.exe'
zig=str(zig_local) if zig_local.exists() else shutil.which('zig')
if not zig: raise SystemExit('Building is optional. Install Zig 0.17.1 for the compact static SQLite build.')
tags='sqlite_omit_load_extension,netgo,osusergo'
env=os.environ.copy()
env.update(GOCACHE=str(ROOT/'build/go-cache'),GOMODCACHE=str(ROOT/'build/go-modules'),
           GOPATH=str(ROOT/'build/go-path'),GOTOOLCHAIN='local',GOTELEMETRY='off',CGO_ENABLED='1',
           CGO_CFLAGS='-Os -g -ffunction-sections -fdata-sections',
           ZIG_LOCAL_CACHE_DIR=str(ROOT/'build/zig-cache'),ZIG_GLOBAL_CACHE_DIR=str(ROOT/'build/zig-global-cache'))
env['GOOS']='windows' if os.name=='nt' else 'linux'
env['GOARCH']='amd64'
env.pop('GOARM',None)
env['PATH']=str(Path(go).parent)+os.pathsep+env.get('PATH','')
host_target='x86_64-windows-gnu' if os.name=='nt' else 'x86_64-linux-musl'
env['CC']='"'+zig.replace('\\','/')+'" cc -target '+host_target
def run(*command,output=False):
    return subprocess.run([go,*command],cwd=ROOT/'native',env=env,check=True,text=True,
                          stdout=subprocess.PIPE if output else None).stdout
if not args.skip_tests:
    run('vet','-tags='+tags,'./...')
    run('test','-tags='+tags,'-count=1','-timeout=90s','./...')
if args.scan: run('run','golang.org/x/vuln/cmd/govulncheck@v1.9.0','-tags='+tags,'./...')
DIST.mkdir(exist_ok=True)
stage=DIST/'neighbourchat.koplugin'
# This is the single generated plugin folder; never remove source/data paths.
assert stage.resolve().is_relative_to(DIST.resolve()) and stage.name=='neighbourchat.koplugin'
def remove_readonly(function,path,error):
    resolved=Path(path).resolve()
    assert resolved==stage.resolve() or resolved.is_relative_to(stage.resolve())
    os.chmod(path,stat.S_IREAD|stat.S_IWRITE|stat.S_IXUSR)
    function(path)
if stage.exists(): shutil.rmtree(stage,onexc=remove_readonly)
shutil.copytree(ROOT/'koreader/neighbourchat.koplugin',stage)
(stage/'bin').mkdir()
binary=stage/'bin/neighbour-chat'
hostenv=env.copy()
env.update(GOOS='linux',GOARCH='arm',GOARM='7')
env['CC']='"'+zig.replace('\\','/')+'" cc -target arm-linux-musleabihf'
run('build','-tags='+tags,'-trimpath','-buildvcs=false',
    '-ldflags=-s -w -buildid= -linkmode=external -extldflags "-static -Wl,--strip-all"','-o',str(binary),'.')
elf=binary.read_bytes()
assert elf[:6]==b'\x7fELF\x01\x01' and struct.unpack_from('<H',elf,18)[0]==40
offset=struct.unpack_from('<I',elf,28)[0]
size,count=struct.unpack_from('<HH',elf,42)
assert all(struct.unpack_from('<I',elf,offset+i*size)[0]!=3 for i in range(count))
licenses=stage/'LICENSES';licenses.mkdir()
compiler_root=Path(go).resolve().parents[1]
if not (compiler_root/'LICENSE').exists(): compiler_root=Path(run('env','GOROOT',output=True).strip())
shutil.copyfile(compiler_root/'LICENSE',licenses/'Go.txt')
zig_root=Path(zig).resolve().parent
shutil.copyfile(zig_root/'LICENSE',licenses/'Zig-runtime.txt')
shutil.copyfile(zig_root/'lib/libc/musl/COPYRIGHT',licenses/'musl.txt')
modules=run('list','-tags='+tags,'-deps','-f','{{if .Module}}{{.Module.Path}}|{{.Module.Dir}}{{end}}','.',output=True)
for entry in sorted(set(modules.splitlines())):
    if '|' not in entry:continue
    name,directory=entry.split('|',1)
    if name=='neighbourchat' or not directory:continue
    for file in Path(directory).iterdir():
        if file.is_file() and file.name.startswith(('LICENSE','COPYING')):
            shutil.copyfile(file,licenses/(name.replace('/','_')+'-'+file.name+'.txt'))
env=hostenv
if args.scan:run('run','golang.org/x/vuln/cmd/govulncheck@v1.9.0','-mode=binary',str(binary))
info={'version':'7.1','target':'linux/arm/v7','static':True,'binary_sha256':hashlib.sha256(elf).hexdigest(),'binary_bytes':len(elf),
      'native_app_installer':False,'kindle_tested':False,'compiler':run('version',output=True).strip(),
      'sqlite':'statically linked C engine; extension loading disabled','strip_c_debug':True}
(stage/'BUILD-INFO.json').write_text(json.dumps(info,indent=2)+'\n',encoding='utf-8')
def write_zip(path,files):
    with zipfile.ZipFile(path,'w',zipfile.ZIP_DEFLATED,compresslevel=9) as z:
        for name,file in sorted(files.items()):
            entry=zipfile.ZipInfo(name);entry.compress_type=zipfile.ZIP_DEFLATED
            entry.external_attr=(0o100755 if name.endswith('.sh') or name.endswith('/bin/neighbour-chat') else 0o100644)<<16
            z.writestr(entry,file.read_bytes())
    Path(str(path)+'.sha256').write_text(hashlib.sha256(path.read_bytes()).hexdigest()+'  '+path.name+'\n',encoding='ascii')
    with zipfile.ZipFile(path) as z:assert z.testzip() is None
files={'neighbourchat.koplugin/'+f.relative_to(stage).as_posix():f for f in stage.rglob('*') if f.is_file()}
assert not any('/data/' in n or n.endswith(('.exe','.pyc','.dll')) for n in files)
write_zip(DIST/'NeighbourChat-KOReader.zip',files)
source={}
for folder in ['native','koreader/neighbourchat.koplugin','koreader/tests']:
    for file in (ROOT/folder).rglob('*'):
        if file.is_file() and '__pycache__' not in file.parts:source[file.relative_to(ROOT).as_posix()]=file
for name in ['tools/build-koreader.py','koreader/INSTALL.txt','README.txt','INSTALL.txt','SECURITY.txt','SECURITY-REVIEW.txt','VERIFICATION.txt']:
    source[name]=ROOT/name
write_zip(DIST/'NeighbourChat-KOReader-Source.zip',source)
print(json.dumps({**info,'plugin_folder':str(stage),'zip_bytes':(DIST/'NeighbourChat-KOReader.zip').stat().st_size},indent=2))
