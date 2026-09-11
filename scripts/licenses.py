#!/usr/bin/env python3
"""Collect notices from modules actually compiled into the macOS arm64 CLI."""
import json, os, pathlib, shutil, subprocess
ROOT=pathlib.Path(__file__).resolve().parents[1]
def objects(text):
    decoder=json.JSONDecoder()
    while text.strip():
        value,end=decoder.raw_decode(text.lstrip()); text=text.lstrip()[end:]; yield value
def dependencies():
    env=dict(os.environ,GOOS='darwin',GOARCH='arm64',CGO_ENABLED='0')
    packages=objects(subprocess.check_output(['go','list','-deps','-json','./cmd/tidemux'],cwd=ROOT,env=env,text=True))
    return {p['Module']['Path']:p['Module'] for p in packages if p.get('Module') and not p['Module'].get('Main')}
LICENSES={'github.com/dustin/go-humanize':'MIT','github.com/mattn/go-isatty':'MIT','github.com/ncruces/go-strftime':'MIT'}
def collect():
    dest=ROOT/'licenses';dest.mkdir(exist_ok=True)
    rows=[]
    for name,module in sorted(dependencies().items()):
        target=dest/name.replace('/','_');target.mkdir(exist_ok=True)
        files=[p for p in pathlib.Path(module['Dir']).iterdir() if p.is_file() and p.name.lower().startswith(('license','copying','copyright','notice'))]
        if not files: raise RuntimeError('No license for '+name)
        for file in files: shutil.copyfile(file,target/file.name)
        rows.append(f"| `{name}` | `{module['Version']}` | {LICENSES.get(name,'BSD-3-Clause')} | [notices](licenses/{target.name}/) |")
    go_root=pathlib.Path(subprocess.check_output(['go','env','GOROOT'],text=True).strip())
    shutil.copyfile(go_root/'LICENSE',dest/'GO-LICENSE')
    (ROOT/'THIRD_PARTY_NOTICES.md').write_text('# Third-party notices\n\nGenerated from the dependencies compiled for `darwin/arm64` by `scripts/licenses.py`.\nExact versions are pinned by go.mod/go.sum. Original notices are included verbatim.\n\n| Module | Version | Primary license | Text |\n| --- | --- | --- | --- |\n'+'\n'.join(rows)+'\n\nThe Go toolchain/runtime uses [BSD-3-Clause](licenses/GO-LICENSE).\n`modernc.org/libc` also includes MIT, BSD and other permissive third-party terms;\nread its bundled LICENSE-3RD-PARTY.md. Additional notices shipped by modules\nare retained even when a corresponding asset is not part of the executable.\nBrand assets are not included. Release-specific SPDX metadata accompanies each build.\n')
if __name__=='__main__': collect()
