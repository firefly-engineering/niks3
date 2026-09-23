# Two derivations for the action's end-to-end test. `final` reads
# `intermediate` at build time but does not reference it, so `intermediate`
# is outside final's runtime closure: it reaches the cache only if the
# action uploads everything the job built, as it must for module downloads
# and vendored trees.
{ seed }:
let
  drv =
    name: script:
    derivation {
      inherit name;
      system = builtins.currentSystem;
      builder = "/bin/sh";
      args = [
        "-c"
        script
      ];
    };
in
rec {
  intermediate = drv "niks3-e2e-intermediate" "echo ${seed} > $out";
  final = drv "niks3-e2e-final" "read x < ${intermediate}; echo \"built from $x\" > $out";
}
