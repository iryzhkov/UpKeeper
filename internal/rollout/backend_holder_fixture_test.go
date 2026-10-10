package rollout

import "strings"

// Restrict inventory to the isolated real fixture process; its status and fd
// permissions/inodes remain real kernel data. This avoids unrelated processes
// on shared workers correctly refusing the scan before reaching the fixture.
func holderFixtureProgram() string {
	start := strings.Index(databaseHolderScript, "import os, sys")
	end := strings.LastIndex(databaseHolderScript, "\nPY")
	return `import os,sys
real_listdir=os.listdir
def listdir(path):
 if path=="/proc": return [sys.argv[2]]
 return real_listdir(path)
os.listdir=listdir
` + databaseHolderScript[start:end]
}
