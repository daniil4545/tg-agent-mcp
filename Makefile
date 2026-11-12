# Go code lives in src/; its statics are in src/Makefile.
.PHONY: lint
lint:
	$(MAKE) -C src lint
	uvx --from shellcheck-py==0.11.0.1 shellcheck deploy/safe-ssh.sh
