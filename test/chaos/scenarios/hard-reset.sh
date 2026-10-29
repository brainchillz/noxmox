# Holder hard reset during writes (sysrq-b). Expect: survivor acquires in
# under 150 s and the verifier data is consistent to the last fsync.
. "$HERE/scenarios/_crash.sh"
scenario_run() { crash_scenario hard-reset crash_node 30 150; }
