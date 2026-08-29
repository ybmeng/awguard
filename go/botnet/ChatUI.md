# ChatUI

Idea-capture doc for the PrivateBotNet chat UI. Write freely — structure below
is a starting skeleton, reshape it as the ideas come.

## Idea

The broad idea of the ChatUI is to describe the interface so that we can build it to call the go service that actually manages the state. This allows us to isolate the interaction UI, and to have solid service contracts that

## Breakdown

Inspired by Grokbot, the interaction UI should be very simple. The basic level of abstraction is the 'bot' which is a simple UI to talk to. To the user it is a singular entity but under the hood it can be arbitrarily complex. Grokbot hides that complexity, but we can expose all of it.

The overall UI should have 
* Left Nav: a persistent left panel that has vertically stacked sections.
* Bot Chat: When a bot is selected from the left nav, then the chat with the bot is opened up on the right.
* Bot Chat Right Panel: This is where various pieces of information can be displayed for the bot

### Left Nav

The left nav should have 2 sections
* Services - this is a list of services that are globally available to all bots, and we'll continue to build these services to enable bot functionality. It should be a collapsible thing that lets us access each service and the customizable UI.
* Bots - This is where we can select bots that are in our botnet.

### Services Panel

When a service is selected we allow modification of the services through this panel. Each service has an individualized UI.

### Bot Chat Panel

This opens up the ability to 'chat with' the bot.


### Bot Chat Panel Right Panel

This is to the right of the bot chat panel, and allows the user to look at key details that the chat was created with, like the system prompt... context length... etc...