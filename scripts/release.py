#!/usr/bin/env python3
"""Build immutable local candidate artifacts; does not upload or publish."""
import datetime, hashlib, json, os, pathlib, re, shutil, subprocess, tarfile, tempfile, uuid
from licenses import dependencies, LICENSES
ROOT=pathlib.Path(__file__).resolve().parents[1]
def run(*args,**kw): return subprocess.check_output(args,cwd=ROOT,text=True,**kw).strip()
def digest(path): return hashlib.sha256(path.read_bytes()).hexdigest()
def main():
    if run('git','status','--porcelain'): raise SystemExit('Commit reviewed changes before packaging.')
    commit=run('git','rev-parse','HEAD')
    version=re.search(r'version\s*=\s*"([^"]+)"',(ROOT/'cmd/tidemux/main.go').read_text())[1]
    if not re.fullmatch(r'\d+\.\d+\.\d+(?:-rc\.\d+)?',version):raise SystemExit('Invalid version')
    dest=ROOT/'dist'/version
    if dest.exists(): raise SystemExit('Version output exists; do not overwrite artifacts.')
    dest.mkdir(parents=True)
    env=dict(os.environ,CGO_ENABLED='0',GOOS='darwin',GOARCH='arm64')
    with tempfile.TemporaryDirectory(prefix='tidemux-release-') as tmp:
        stage=pathlib.Path(tmp)/f'tidemux-{version}';stage.mkdir()
        binary=stage/'tidemux'
        subprocess.run(['go','build','-trimpath','-buildvcs=true','-o',str(binary),'./cmd/tidemux'],cwd=ROOT,env=env,check=True)
        for name in ['LICENSE','NOTICE','README.md','PRIVACY.md','SECURITY.md','THIRD_PARTY_NOTICES.md','CHANGELOG.md','SBOM.md','tidemux.example.json']:
            shutil.copyfile(ROOT/name,stage/name)
        for name in ['docs','examples','licenses']:shutil.copytree(ROOT/name,stage/name)
        info=run('go','version','-m',str(binary));(stage/'BUILD.txt').write_text(f'commit: {commit}\n'+info+'\n')
        timestamp=datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
        rootid='SPDXRef-TideMux'
        packages=[{'SPDXID':rootid,'name':'TideMux','versionInfo':version,'downloadLocation':'NOASSERTION','filesAnalyzed':False,'licenseConcluded':'Apache-2.0','licenseDeclared':'Apache-2.0','copyrightText':'NOASSERTION','checksums':[{'algorithm':'SHA256','checksumValue':digest(binary)}],'sourceInfo':'git commit '+commit}]
        relationships=[{'spdxElementId':'SPDXRef-DOCUMENT','relationshipType':'DESCRIBES','relatedSpdxElement':rootid}]
        for i,(name,mod) in enumerate(sorted(dependencies().items())):
            ident=f'SPDXRef-module-{i}'
            packages.append({'SPDXID':ident,'name':name,'versionInfo':mod['Version'],'downloadLocation':'NOASSERTION','filesAnalyzed':False,'licenseConcluded':'NOASSERTION','licenseDeclared':LICENSES.get(name,'BSD-3-Clause'),'copyrightText':'NOASSERTION','sourceInfo':'Go module sum '+mod.get('Sum','unknown')+'; original and nested notices in licenses/','externalRefs':[{'referenceCategory':'PACKAGE-MANAGER','referenceType':'purl','referenceLocator':f"pkg:golang/{name}@{mod['Version']}"}]})
            relationships.append({'spdxElementId':rootid,'relationshipType':'DEPENDS_ON','relatedSpdxElement':ident})
        packages.append({'SPDXID':'SPDXRef-Go','name':'Go runtime','versionInfo':run('go','env','GOVERSION'),'downloadLocation':'https://go.dev/dl/','filesAnalyzed':False,'licenseConcluded':'BSD-3-Clause','licenseDeclared':'BSD-3-Clause','copyrightText':'Copyright The Go Authors'})
        relationships.append({'spdxElementId':rootid,'relationshipType':'DEPENDS_ON','relatedSpdxElement':'SPDXRef-Go'})
        spdx={'spdxVersion':'SPDX-2.3','dataLicense':'CC0-1.0','SPDXID':'SPDXRef-DOCUMENT','name':f'TideMux-{version}-darwin-arm64','documentNamespace':f'https://github.com/hs3180/tidemux/sbom/{uuid.uuid5(uuid.NAMESPACE_URL,commit+digest(binary))}','creationInfo':{'creators':['Tool: tidemux-release.py'],'created':timestamp},'packages':packages,'relationships':relationships}
        (stage/'sbom.spdx.json').write_text(json.dumps(spdx,indent=2)+'\n')
        asset=dest/f'tidemux_{version}_darwin_arm64.tar.gz'
        with tarfile.open(asset,'w:gz') as archive:archive.add(stage,arcname=stage.name)
        for name in ['sbom.spdx.json','BUILD.txt']:shutil.copyfile(stage/name,dest/name)
    sha=digest(asset)
    formula=f'''class Tidemux < Formula
  desc "Local OpenAI and Anthropic compatible API gateway"
  homepage "https://github.com/hs3180/tidemux"
  url "https://github.com/hs3180/tidemux/releases/download/v{version}/{asset.name}"
  version "{version}"
  sha256 "{sha}"
  license "Apache-2.0"
  depends_on macos: :sequoia
  depends_on arch: :arm64

  def install
    bin.install "tidemux"
    pkgshare.install "tidemux.example.json", "examples", "docs", "licenses", "sbom.spdx.json", "BUILD.txt", "LICENSE", "NOTICE", "THIRD_PARTY_NOTICES.md"
  end

  test do
    assert_equal "{version}", shell_output("#{{bin}}/tidemux version").strip
  end
end
'''
    (dest/'tidemux.rb').write_text(formula)
    (dest/'SHA256SUMS').write_text(''.join(f'{digest(p)}  {p.name}\n' for p in sorted(dest.iterdir()) if p.is_file()))
    print(dest)
if __name__=='__main__':main()
