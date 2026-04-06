# Vif

A package manager for PHP similar to [Composer](https://getcomposer.org/)

> Note: If it wasn't obvious, this project is not even in alpha state, since we're still missing a bunch of commands. I'd appreciate it if you ran `vif update` against your php project and tell me if it matches composer. The more debug info, the better!

## Install

```sh
go install github.com/van-sprundel/vif@latest
```

## Background

I recently got a job where I had to use PHP again. Think old Drupal and Symfony projects that now need to be refactored. 

I found the timings on `composer update` to be insane. On top of that, I only got `composer update` errors  at the **end** of the command (at cleanup phase), meaning I have to wait two minutes on something that had failed anyway. 

For this project we use the pubgrub algorithm, which can just stop and panic if it cannot find a solution.

## Motivation

I've seen what [bun](https://bun.sh) and [uv](https://docs.astral.sh/uv/) have done to their ecosystem. I had a similar idea back in 2022, but never got around to it. It had left my mind when I joined a TS company, but I now that im back on PHP island I realize *why* I wanted to do this project in the first place... I hate waiting!

I do not expect this project to be succesful in any way. It's more a way to prove me wrong how complex the issue of "resolving+installing packages" can get.

Also, LLMs were only used for doing some of the source gathering. I am not planning to use LLMs to code.

^ I ended up writing the bench and compat harness with Claude, since I wasn't sure how to fairly compare Composer against Vif.

## Showcase

```bash
> time composer update
Loading composer repositories with package information
Updating dependencies
Lock file operations: 0 installs, 52 updates, 0 removals
... (truncated)
No security vulnerability advisories found.

No security vulnerability advisories found.

________________________________________________________
Executed in   17.99 secs    fish           external
   usr time    4.11 secs  588.00 micros    4.11 secs
   sys time    1.62 secs  379.00 micros    1.62 secs

# Now let's do vif

> time ./vif update
Resolving [207]
Resolving done (0 packages, 7.14s)
Resolved 142 packages
Lock file operations: 0 installs, 52 updates, 0 removals, 90 unchanged
Downloading [142/142]
Downloading done (142 packages, 4ms)
  0 downloaded, 141 from cache
Installing to vendor...
  141 installed, 0 updated, 0 removed, 0 skipped
Generating autoload files... done
Wrote composer.lock

vif install completed: 142 packages in 8.42s

________________________________________________________
Executed in    8.45 secs    fish           external
   usr time   31.45 secs  503.00 micros   31.45 secs
   sys time    1.87 secs  352.00 micros    1.87 secs
```

We're even able to resolve in certain situations where composer is not a fan of our constraints

```sh
> time composer update
More deprecation notices were hidden, run again with `-v` to show them.
Gathering patches for root package.
Loading composer repositories with package information
Updating dependencies
Your requirements could not be resolved to an installable set of packages.

  Problem 1
    - Root composer.json requires drupal/core-recommended ^9.3 -> satisfiable by drupal/core-recommended[9.3.x-dev, 9.4.x-dev, 9.5.x-dev].
    - drupal/core-recommended 9.3.x-dev requires symfony/http-foundation v4.4.34 -> found symfony/http-foundation[v4.4.34] but these were not loaded, because they are affected by security advisories ("PKSA-365x-2zjk-pt47", "PKSA-b35n-565h-rs4q"). Go to https://packagist.org/security-advisories/ to find advisory details. To ignore the advisories, add them to the audit "ignore" config. To turn the feature off entirely, you can set "block-insecure" to false in your "audit" config.
    - drupal/core-recommended 9.4.x-dev requires twig/twig ~v2.15.3 -> found twig/twig[v2.15.3, v2.15.4, v2.15.5, v2.15.6] but these were not loaded, because they are affected by security advisories ("PKSA-yhcn-xrg3-68b1", "PKSA-2wrf-1xmk-1pky", "PKSA-6319-ffpf-gx66"). Go to https://packagist.org/security-advisories/ to find advisory details. To ignore the advisories, add them to the audit "ignore" config. To turn the feature off entirely, you can set "block-insecure" to false in your "audit" config.
    - drupal/core-recommended 9.5.x-dev requires twig/twig ~v2.15.4 -> found twig/twig[v2.15.4, v2.15.5, v2.15.6] but these were not loaded, because they are affected by security advisories ("PKSA-yhcn-xrg3-68b1", "PKSA-2wrf-1xmk-1pky", "PKSA-6319-ffpf-gx66"). Go to https://packagist.org/security-advisories/ to find advisory details. To ignore the advisories, add them to the audit "ignore" config. To turn the feature off entirely, you can set "block-insecure" to false in your "audit" config.

Use the option --with-all-dependencies (-W) to allow upgrades, downgrades and removals for packages currently locked to specific versions.

________________________________________________________
Executed in  113.39 secs    fish           external
   usr time    4.32 secs  719.00 micros    4.32 secs
   sys time    0.31 secs  425.00 micros    0.31 secs

# Now let's try vif

> time ./vif update
Resolving dependencies for drupal-composer/drupal-project...
Resolving [396]
Resolving done (0 packages, 85.87s)
resolve: resolver: no version of drupal/core-recommended could satisfy all constraints

________________________________________________________
Executed in  85.93 secs    fish           external
   usr time   35.55 secs  746.00 micros   35.55 secs
   sys time    1.85 secs  830.00 micros    1.85 secs
```

While it is *technically* faster, the current resolve is not fast at all... we need to do a deeper dive into why, but that's for later.

## Sources

- [PubGrub](https://github.com/pubgrub-rs/pubgrub) version solving algorithm that's also used in uv. Made a big speed bump
- [Packagist API](https://packagist.com/docs/api) we use this at work 
- [Composer](https://getcomposer.org/doc/04-schema.md) thank god that this exists
- also bun and uv as inspo 

I've also been testing against the [Drupal composer.lock](https://git.drupalcode.org/project/drupal/-/blob/main/composer.lock), so shoutout to Drupal.

## Final thoughts

Project is fairly close to composer's behavior, but we're still missing a bunch of subcommands(require, recipes, etc.) and features like script exec (e.g. postinstall).
