{
  testers,
  writeText,
  s5cmd,
  rustfs,
  pkgs,
  ...
}:

# A client that trusts only niks3 substitutes a path that exists only in an
# upstream cache; niks3 pulls it, checks the upstream signature and re-signs
# it. A path signed by a different key under the upstream's key name is
# refused.
let
  niks3SecretKey = writeText "niks3-signing-key" "niks3-test-1:0knWkx/F+6IJmI4dkvNs14SCaewg9ZWSAQUNg9juRxh/8x+rzUJx9SWdyGOVl21IbJlQemUKG40qW2TTyrE++w==";
  niks3PublicKey = "niks3-test-1:f/Mfq81CcfUlnchjlZdtSGyZUHplChuNKltk08qxPvs=";
  upstreamSecretKey = writeText "upstream-signing-key" "upstream-test-1:XEhvUWaT5MWVNCp23mUPnOeaiw5sLmVmE+NkaysvkQJN0UbALfgeIc0mboLg64XVoou1aTWuimkw1pOkUsGW9A==";
  upstreamPublicKey = "upstream-test-1:TdFGwC34HiHNJm6C4OuF1aKLtWk1roppMNaTpFLBlvQ=";
  # Same key name as the upstream's, different key.
  rogueSecretKey = writeText "rogue-signing-key" "upstream-test-1:ltHX5JA37NWjWwyxrwr5v+QCZKvk7u/TSjBfzFVV56oNh6dPIDunVc1VAh0zTexfYo5S2PoFdfmRkLeWyssFtw==";
in
testers.nixosTest {
  name = "nixos-test-upstream";

  nodes.upstream = {
    nix.settings.experimental-features = [ "nix-command" ];
    services.nginx = {
      enable = true;
      virtualHosts.upstream.root = "/srv/cache";
    };
    networking.firewall.allowedTCPPorts = [ 80 ];
    systemd.tmpfiles.rules = [ "d /srv/cache 0755 root root -" ];
  };

  nodes.server = {
    imports = [ ../nixosModules/niks3.nix ];

    services.niks3 = {
      enable = true;
      httpAddr = "0.0.0.0:5751";
      s3 = {
        endpoint = "localhost:9000";
        bucket = "niks3-test";
        useSSL = false;
        accessKeyFile = writeText "s3-access-key" "rustfsadmin";
        secretKeyFile = writeText "s3-secret-key" "rustfsadmin";
      };
      apiTokenFile = writeText "api-token" "test-token-that-is-at-least-36-characters-long";
      signKeyFiles = [ niks3SecretKey ];
      readProxy = {
        enable = true;
        upstreams = [
          {
            url = "http://upstream";
            publicKey = upstreamPublicKey;
          }
        ];
        # Stands in for the HTTP Basic auth proxy of a real deployment.
        gatedByProxy = true;
      };
    };

    networking.firewall.allowedTCPPorts = [ 5751 ];

    systemd.services.rustfs = {
      after = [ "network.target" ];
      wantedBy = [ "multi-user.target" ];
      serviceConfig = {
        ExecStart = "${rustfs}/bin/rustfs --address 0.0.0.0:9000 --access-key rustfsadmin --secret-key rustfsadmin /var/lib/rustfs";
        StateDirectory = "rustfs";
        DynamicUser = true;
      };
    };

    systemd.services.rustfs-setup = {
      after = [ "rustfs.service" ];
      requires = [ "rustfs.service" ];
      before = [ "niks3.service" ];
      wantedBy = [ "multi-user.target" ];
      environment = {
        S3_ENDPOINT_URL = "http://localhost:9000";
        AWS_ACCESS_KEY_ID = "rustfsadmin";
        AWS_SECRET_ACCESS_KEY = "rustfsadmin";
      };
      path = [ s5cmd ];
      script = ''
        for i in $(seq 60); do s5cmd ls 2>/dev/null && break; sleep 2; done
        s5cmd mb s3://niks3-test || true
      '';
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
      };
    };

    systemd.services.niks3 = {
      after = [ "rustfs-setup.service" ];
      requires = [ "rustfs-setup.service" ];
    };
  };

  nodes.client = {
    nix.settings = {
      substituters = pkgs.lib.mkForce [ "http://server:5751" ];
      trusted-public-keys = pkgs.lib.mkForce [ niks3PublicKey ];
    };
    environment.systemPackages = [ pkgs.curl ];
  };

  testScript = ''
    def build(name):
        return upstream.succeed(
            "nix-build --no-out-link -E 'derivation { name=\"" + name + "\"; system=builtins.currentSystem; "
            + "builder=\"/bin/sh\"; args=[\"-c\" \"echo " + name + " > $out\"]; }'"
        ).strip()

    def publish(path, key):
        upstream.succeed(f"nix copy --to 'file:///srv/cache?compression=zstd&secret-key={key}' {path}")

    start_all()
    upstream.wait_for_unit("nginx.service")
    server.wait_for_unit("niks3.service")
    server.wait_for_open_port(5751)

    good = build("from-upstream")
    publish(good, "${upstreamSecretKey}")
    rogue = build("rogue-signed")
    publish(rogue, "${rogueSecretKey}")

    with subtest("client trusting only niks3 substitutes a path only the upstream has"):
        client.fail(f"test -e {good}")
        client.succeed(f"nix-store --realise {good}")
        client.succeed(f"grep from-upstream {good}")
        sigs = client.succeed(f"nix path-info --extra-experimental-features nix-command --sigs --json --store http://server:5751 {good}")
        assert "niks3-test-1:" in sigs, sigs

    with subtest("a narinfo with a bad upstream signature is refused"):
        client.fail(f"nix-store --realise {rogue}")
        h = rogue.removeprefix("/nix/store/")[:32]
        client.succeed(f"test $(curl -so /dev/null -w '%{{http_code}}' http://server:5751/{h}.narinfo) = 404")
        server.succeed("journalctl -u niks3 | grep 'Refusing upstream narinfo'")
  '';
}
