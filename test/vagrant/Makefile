# Development commands for the Vagrant environment. Build/deploy from the root
# with make develop e2e-images before running these tests.
.PHONY: sync test e2e

sync:
	bash sync.sh

test:
	./transport.sh ssh controller 'export PATH="$$HOME/.skiff/tools/$$(uname -m)/bin:$$PATH"; bash $$HOME/skiff/test/regression.sh'

e2e:
	./transport.sh ssh controller 'export PATH="$$HOME/.skiff/tools/$$(uname -m)/bin:$$PATH"; cd $$HOME/skiff && SKIFF_DEV=1 bash test/e2e/run-all.sh $(E2E_ARGS)'
