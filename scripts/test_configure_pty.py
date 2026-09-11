#!/usr/bin/env python3
"""PTY onboarding checks: unlocked, successful unlock, failed unlock, cancellation."""
import json, os, pathlib, pty, select, signal, subprocess, tempfile, time
ROOT=pathlib.Path(__file__).resolve().parents[1]
with tempfile.TemporaryDirectory(prefix='tidemux-configure-test-') as temp:
    root=pathlib.Path(temp);binary=os.environ.get('TIDEMUX_TEST_BINARY',str(root/'tidemux'))
    if 'TIDEMUX_TEST_BINARY' not in os.environ:subprocess.run(['go','build','-o',binary,'./cmd/tidemux'],cwd=ROOT,check=True)
    fake=root/'bin';fake.mkdir()
    (fake/'security').write_text('''#!/usr/bin/env python3
import sys,os,json,shlex,pathlib,getpass
p=pathlib.Path(os.environ['TIDEMUX_TEST_KEYCHAIN_DB']);marker=p.with_suffix('.unlocked')
mode=os.environ['TIDEMUX_TEST_UNLOCK'];d=json.loads(p.read_text()) if p.exists() else {};a=sys.argv[1:]
if a==['-i']:
 a=shlex.split(sys.stdin.read());key=a[a.index('-s')+1]+'/'+a[a.index('-a')+1]
 if key in d:sys.exit(1)
 d[key]=bytes.fromhex(a[a.index('-X')+1]).decode();p.write_text(json.dumps(d))
elif a==['show-keychain-info']:
 sys.exit(0 if mode=='unlocked' or marker.exists() else 1)
elif a==['unlock-keychain']:
 if mode=='unlocked':sys.exit(5)
 try:password=getpass.getpass('System unlock password: ')
 except (KeyboardInterrupt,EOFError):sys.exit(1)
 if mode=='failure' or password!='synthetic-login-password':sys.exit(1)
 marker.touch()
elif a[0]=='find-generic-password':
 key=a[a.index('-s')+1]+'/'+a[a.index('-a')+1]
 if key not in d:sys.exit(1)
 print(d[key])
elif a[0]=='delete-generic-password':
 key=a[a.index('-s')+1]+'/'+a[a.index('-a')+1];d.pop(key,None);p.write_text(json.dumps(d))
else:sys.exit(2)
''');(fake/'security').chmod(0o700)
    for mode in ['unlocked','success','failure','cancel']:
        db=root/(mode+'-secrets.json');config=root/mode/'config.json'
        env=dict(os.environ,PATH=str(fake)+':'+os.environ['PATH'],TIDEMUX_TEST_KEYCHAIN_DB=str(db),TIDEMUX_TEST_UNLOCK=mode)
        secret=b'synthetic-pty-secret-not-real';password=b'synthetic-login-password'
        pid,fd=pty.fork()
        if pid==0:os.execve(binary,[binary,'configure','--preset','deepseek','--config',str(config)],env)
        captured=b'';sent=False;unlocked=False;deadline=time.monotonic()+15;status=None
        try:
            while time.monotonic()<deadline:
                ready,_,_=select.select([fd],[],[],0.1)
                if ready:
                    try:chunk=os.read(fd,8192)
                    except OSError:chunk=b''
                    captured+=chunk
                    if not unlocked and b'System unlock password:' in captured:
                        time.sleep(0.05);os.write(fd,b'\x03' if mode=='cancel' else password+b'\n');unlocked=True
                    if not sent and b'API key (hidden' in captured:
                        time.sleep(0.05);os.write(fd,secret+b'\n');sent=True
                done,result=os.waitpid(pid,os.WNOHANG)
                if done:status=result;break
            if status is None:os.kill(pid,signal.SIGKILL);os.waitpid(pid,0);raise RuntimeError('configure timeout')
        finally:os.close(fd)
        assert secret not in captured and password not in captured, 'credential echoed'
        if mode != 'unlocked':
            assert b'Your login keychain is locked.' in captured, 'missing English unlock guidance'
        if mode == 'success':
            assert b'Keychain unlocked. Continuing setup.' in captured, 'missing English confirmation'
        if mode in ['failure','cancel']:
            assert status!=0 and not sent and not config.exists() and not db.exists(), 'failed unlock mutated configuration'
        else:
            assert status==0, 'configure failed with synthetic Keychain'
            assert unlocked==(mode=='success'), 'unnecessary or missing unlock prompt'
            c=json.loads(config.read_text());assert secret.decode() not in config.read_text()
            assert c['model']=='deepseek-flash' and c['base_url']=='https://api.deepseek.com'
            assert config.stat().st_mode & 0o777 == 0o600
            values=json.loads(db.read_text());assert len(values)==2 and secret.decode() in values.values()
            assert 'Local checks passed' in captured.decode()
        print('PTY configure passed: '+mode)
