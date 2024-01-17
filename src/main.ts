import { Bot } from "https://deno.land/x/grammy@v1.20.3/mod.ts";

const token = Deno.env.get("BOT_TOKEN")
if (!token) {
    throw new Error("BOT_TOKEN must be provided")
}

const bot = new Bot(token);

bot.command("start", (ctx) => ctx.reply("Welcome! Up and running."));

bot.on("message", (ctx) => ctx.reply("Got another message!"));

bot.start();