# Instructions for agents

You are setting up or operating the Perfloop relay in a customer's network.

- Follow [docs/setup.md](docs/setup.md) in order. Do not skip a checkpoint;
  each one is the observable that proves the step worked.
- Never print, log, or write a token to a file. Tokens go from the operator's
  paste into a Secret through a shell variable, as the procedure shows.
- Before applying anything, run `-print-routes` (step 3) and keep its output.
  It is the complete list of what the relay can be asked to do.
- The relay token and the source registration are created in Perfloop Setup
  by a tenant admin in the browser. If you are not that admin, stop at those
  steps and ask for the token and the confirmation, do not work around them.
- Pin the image by digest. Never use a mutable tag in a manifest.
- What the relay does and refuses is in [docs/security.md](docs/security.md);
  configuration fields in [docs/configuration.md](docs/configuration.md);
  logs and `-print-routes` in [docs/operations.md](docs/operations.md);
  image verification in [docs/image.md](docs/image.md).
- When something fails, use the table at the end of `docs/setup.md` before
  changing anything else.
