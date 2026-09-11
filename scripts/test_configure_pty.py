#!/usr/bin/env python3
"""CLI onboarding regression using a PTY and a synthetic Keychain double."""
import json, os, pathlib, pty, select, signal, subprocess, sys, tempfile, time
ROOT=pathlib.Path(__file__).resolve().parents[1]
with tempfile.TemporaryDirectory(prefix='tidemux-configure-test-') as temp:
    root=pathlib.Path(temp);binary=os.environ.get('TIDEMUX_TEST_BINARY',str(root/'tidemux'))
    if 'TIDEMUX_TEST_BINARY' not in os.environ:subprocess.run(['go','build','-o',binary,'./cmd/tidemux'],cwd=ROOT,check=True)
    fake=root/'bin';fake.mkdir();db=root/'synthetic-secrets.json'
    (fake/'security').write_text('''#!/usr/bin/env python3
import sys,os,json,shlex,pathlib
p=pathlib.Path(os.environ['TIDEMUX_TEST_KEYCHAIN_DB'])
d=json.loads(p.read_text()) if p.exists() else {}
a=sys.argv[1:]
if a==['-i']:
 a=shlex.split(sys.stdin.read());key=a[a.index('-s')+1]+'/'+a[a.index('-a')+1]
 if key in d:sys.exit(1)
 d[key]=bytes.fromhex(a[a.index('-X')+1]).decode();p.write_text(json.dumps(d))
elif a[0]=='show-keychain-info':pass
elif a[0]=='find-generic-password':
 key=a[a.index('-s')+1]+'/'+a[a.index('-a')+1]
 if key not in d:sys.exit(1)
 print(d[key])
elif a[0]=='delete-generic-password':
 key=a[a.index('-s')+1]+'/'+a[a.index('-a')+1];d.pop(key,None);p.write_text(json.dumps(d))
else:sys.exit(2)
''');(fake/'security').chmod(0o700)
    env=dict(os.environ,PATH=str(fake)+':'+os.environ['PATH'],TIDEMUX_TEST_KEYCHAIN_DB=str(db))
    config=root/'app'/'config.json';secret=b'synthetic-pty-secret-not-real'
    pid,fd=pty.fork()
    if pid==0:os.execve(binary,[binary,'configure','--preset','deepseek','--config',str(config)],env)
    captured=b'';sent=False;deadline=time.monotonic()+15;status=None
    try:
        while time.monotonic()<deadline:
            ready,_,_=select.select([fd],[],[],0.1)
            if ready:
                try:chunk=os.read(fd,8192)
                except OSError:chunk=b''
                captured+=chunk
                if not sent and b'API key (hidden' in captured:
                    # ReadPassword switches echo off immediately after the prompt.
                    time.sleep(0.05);os.write(fd,secret+b'\n');sent=True
            done,result=os.waitpid(pid,os.WNOHANG)
            if done:status=result;break
        if status is None:os.kill(pid,signal.SIGKILL);os.waitpid(pid,0);raise RuntimeError('configure timeout')
    finally:os.close(fd)
    assert status==0, 'configure failed with synthetic Keychain'
    assert secret not in captured, 'secret echoed to terminal'
    c=json.loads(config.read_text());assert secret.decode() not in config.read_text()
    assert c['model']=='deepseek-flash' and c['base_url']=='https://api.deepseek.com'
    assert config.stat().st_mode & 0o777 == 0o600
    values=json.loads(db.read_text());assert len(values)==2 and secret.decode() in values.values()
    assert 'Local checks passed' in captured.decode()
    print('PTY configure passed: hidden input, two Keychain references, private config, no API call.')
