# config-reload-operator

Signal-based configuration reloads for Kubernetes workloads.

> Work in progress. This README grows with the project.

## Why

Many long-running applications cannot notice when their configuration files
change, but they do reload their configuration when they receive a signal,
usually `SIGHUP`. NGINX, Apache httpd, PostgreSQL, pgbouncer and Fluentd all
work this way.

On Kubernetes, the usual answer to a changed ConfigMap is to roll the
Deployment, which replaces every Pod. For an application that can reload in
place, that is more disruption than the change deserves.

## What

`config-reload-operator` lets you declare, with a `ConfigReloadPolicy`
resource, that a container should be reloaded by signal when its
ConfigMap-mounted configuration changes. A small `watcher` inside the
container detects the change and sends the signal. The application reloads
itself. The Pod stays as it is.

The reload itself is the application's feature. This project provides the
change detection, the policy, the wiring and the failure handling around it.
