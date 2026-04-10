# Vif

A package manager for PHP similar to [Composer](https://getcomposer.org/)

> [!NOTE]
> If it wasn't obvious, this project is not even in alpha state, since we're still missing a bunch of commands. I'd appreciate it if you ran `vif update` against your php project and tell me if it matches composer. The more debug info, the better!

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/van-sprundel/vif/master/install.sh | sh
```

Or with Go:

```sh
go install github.com/van-sprundel/vif@latest
```

## Background

I recently got a job where I had to use PHP again. Think old Drupal and Symfony projects that now need to be refactored. 

I found the timings on `composer update` to be insane. On top of that, I only got `composer update` errors  at the **end** of the command (at cleanup phase), meaning I have to wait two minutes on something that had failed anyway. 

## Motivation

I've seen what [bun](https://bun.sh) and [uv](https://docs.astral.sh/uv/) have done to their ecosystem. I had a similar idea back in 2022, but never got around to it. It had left my mind when I joined a TS company, but I now that im back on PHP island I realize *why* I wanted to do this project in the first place... I hate waiting!

I do not expect this project to be succesful in any way. It's more a way to prove me wrong how complex the issue of "resolving+installing packages" can get.

Also, LLMs were only used for doing some of the source gathering. I am not planning to use LLMs to code.

^ I ended up writing the bench and compat harness with Claude, since I wasn't sure how to fairly compare Composer against Vif.

## Showcase

I had let Claude ran `{composer|vif} update` against a few projects, and it had these key takeaways:

```bash
  Composer vs vif v0.4.2

  ┌──────────┬────────────┬─────────────┬───────────────────────────────────────────────────────┐
  │ Composer │ vif v0.4.2 │ faster?     │                         Notes                         │
  ├──────────┼────────────┼─────────────┼───────────────────────────────────────────────────────┤
  │ 42s      │ 31s        │ 1.4x        │ Composer hit GitLab 429 rate limit                    │
  ├──────────┼────────────┼─────────────┼───────────────────────────────────────────────────────┤
  │ 50s      │ 32s        │ 1.6x        │ Composer: unresolvable deps                           │
  ├──────────┼────────────┼─────────────┼───────────────────────────────────────────────────────┤
  │ 48s      │ 6s         │ 8x          │ Both errored, vif bailed faster                       │
  ├──────────┼────────────┼─────────────┼───────────────────────────────────────────────────────┤
  │ 40s      │ 47s        │ 0.9x        │ vif slower here                                       │
  ├──────────┼────────────┼─────────────┼───────────────────────────────────────────────────────┤
  │ 39s      │ 21s        │ 1.9x        │ Both hit issues                                       │
  ├──────────┼────────────┼─────────────┼───────────────────────────────────────────────────────┤
  │ 1m29s    │ 31s        │ 2.9x        │ Only repo where both actually worked                  │
  ├──────────┼────────────┼─────────────┼───────────────────────────────────────────────────────┤
  │ 5s       │ —          │ —           │ Composer resolved (but unresolvable), vif parse error │
  ├──────────┼────────────┼─────────────┼───────────────────────────────────────────────────────┤
  │ 35s      │ 15s        │ 2.3x        │ Both errored                                          │
  ├──────────┼────────────┼─────────────┼───────────────────────────────────────────────────────┤
  │ 39s      │ 15s        │ 2.6x        │ Both errored                                          │
  └──────────┴────────────┴─────────────┴───────────────────────────────────────────────────────┘
```

Right now vif is running into a bunch of scripting errors. Granted, it stopped early when possible.

## Sources

- [PubGrub](https://github.com/pubgrub-rs/pubgrub) version solving algorithm that's also used in uv. Made a big speed bump
- [Packagist API](https://packagist.com/docs/api) we use this at work 
- [Composer](https://getcomposer.org/doc/04-schema.md) thank god that this exists
- also bun and uv as inspo 

I've also been testing against the [Drupal composer.lock](https://git.drupalcode.org/project/drupal/-/blob/main/composer.lock), so shoutout to Drupal.

## Final thoughts

Project is fairly close to composer's behavior, but we're still missing a bunch of subcommands(require, recipes, etc.) and features like script exec (e.g. postinstall).
