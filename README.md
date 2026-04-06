# Vif

A package manager for PHP similar to [Composer](https://getcomposer.org/)

## Background

I recently got a job where I had to use PHP again. Think old Drupal and Symfony projects that now need to be refactored. I found the timings on `composer update` to be insane. On top of that, I only got composer's issues at the end, meaning I had wasted two minutes waiting on something that had failed anyway.

I've seen what [bun](https://bun.sh) and [uv](https://docs.astral.sh/uv/) have done to their ecosystem. I had a similar idea back in 2022, but never got around to it. It had left my mind when I joined a TS company, but I now that im back on PHP islane I realize why I wanted to improve composer in the first place... I hate waiting!

I do not expect this project to be succesful in any way. It's more a way to prove me wrong how complex the issue of "resolving+installing packages" can get.

Also, LLMs were only used for doing some of the source gathering. I am not planning to use LLMs to code

## Sources

- [PubGrub](https://github.com/pubgrub-rs/pubgrub) version solving algorithm that's also used in uv. Made a big speed bump
- [Packagist API](https://packagist.com/docs/api) we use this at work 
- [Composer](https://getcomposer.org/doc/04-schema.md) thank god that this exists
- also bun and uv as inspo 

I've also been testing against the [Drupal composer.lock](https://git.drupalcode.org/project/drupal/-/blob/main/composer.lock), so shoutout to Drupal.

## Final thoughts

Project is fairly close to composer's behavior, but we're still missing a bunch of subcommands(require, recipes, etc.) and features like script exec (e.g. postinstall).
