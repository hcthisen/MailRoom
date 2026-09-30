# Chat-eksport: Self-hosted email / GHL-alternativ

Eksporteret: 2026-09-29

---

## Bruger — 2026-09-13 15:30 (Europe/Copenhagen)

Hvis jeg vil selfhoste noget der minder om mailgun til at udskifte GHL, hvordan skal det så sættes op?  
Der er 2 dele jeg bruger GHL til, 1 er at sende e-mails automatisk ved trial signup osv.  
Det andet er campaigns som går ud til hele listen, som er omkring 4000 kontakter.

## ChatGPT

Det giver bedst mening at skille det i **transactional mail** og **campaigns**, selv om de kan dele samme mail-infrastruktur. Jeg ville ikke forsøge at erstatte hele GHL med ét system; det bliver både mere simpelt og mere robust med 2–3 små komponenter.

Til en liste på ca. **4.000 kontakter** ville jeg faktisk ikke self-hoste selve SMTP-leveringen i første omgang. Self-host UI, automations og data — men lad fx **Amazon SES** stå for sidste hop til Gmail/Outlook. AWS anbefaler selv shared IP ved lavere/uregelmæssige volumener; en ny dedikeret IP kræver reputation/warmup.

Jeg ville sætte det op sådan her:

```text
Din app / website
        │
        ├── signup / trial / payment events
        ▼
      n8n
        │
        ▼
    listmonk
   ┌────┴─────┐
   │          │
Automations  Campaigns
   │          │
   └────┬─────┘
        ▼
    Amazon SES
        │
        ▼
 Gmail / Outlook / etc.
```

### De enkelte dele

**1. listmonk = erstatning for campaign-delen af GHL**

listmonk er self-hosted og lavet specifikt til mailinglister/newsletters. Det understøtter subscribers, segmenter, templates, campaigns, tracking, unsubscribe og transactional mails via API.

Dine ~4.000 kontakter ligger her.

Eksempel:

```text
List: Newsletter

user@example.com
name: Peter
attributes:
  plan: trial
  language: da
  trial_end: 2026-09-20
  customer: false
```

Så kan du sende:

```text
Alle
Trial users
Betalende kunder
Danske brugere
Trial udløber snart
osv.
```

---

**2. n8n = automations fra GHL**

Når nogen starter trial:

```text
POST /trial-created
       │
       ▼
      n8n
       │
       ├─ Add/update subscriber i listmonk
       │
       ├─ Send "Velkommen til din trial"
       │
       ├─ Vent / schedule
       │
       ├─ Check om bruger stadig er trial
       │
       └─ Send næste email
```

n8n kan self-hostes og bruges netop til webhook/API-baserede workflows.

For eksempel:

```text
Trial signup
   ↓
Email 1: med det samme
   ↓
2 dage
   ↓
Er kunden blevet betalende?
   ├─ Ja → stop
   └─ Nej → Email 2
              ↓
            3 dage
              ↓
           Email 3
```

Til helt simple mails kan din app også bare kalde listmonks transactional API direkte:

```http
POST /api/tx
```

listmonk understøtter transactional templates og dynamiske data direkte via API'et.

---

### 3. Selve mailserveren

Her ville jeg **ikke self-hoste i første omgang**.

Brug:

```text
listmonk
    ↓ SMTP
Amazon SES
    ↓
modtager
```

Du self-hoster altså alt det interessante:

```text
✓ kontaktliste
✓ campaigns
✓ templates
✓ automations
✓ tracking
✓ data
✓ unsubscribe
```

men AWS står for selve SMTP delivery.

Ved dit volumen er SES ekstremt billigt. À-la-carte-prisen er aktuelt **$0.10 / 1.000 udgående mails**, så en campaign til 4.000 personer er omkring **$0.40** i ren sending.

---

## Bruger — 2026-09-13 15:32 (Europe/Copenhagen)

Er Amazon SES den eneste service?  
Eller er det kun fordi den er billigst og bedst

## ChatGPT

Nej. **Amazon SES er langt fra den eneste service.** Jeg foreslog den primært fordi den passer godt til dit setup: meget billig, stabil, SMTP/API, god integration med listmonk og fin deliverability, hvis DNS/reputation er sat korrekt op.

De mest relevante alternativer for dig er:

| Service | Ca. pris | Fordel | Ulempe |
|---|---:|---|---|
| **Amazon SES** | fra $0.10/1.000 mails à-la-carte | Ekstremt billig og skalerbar | AWS-interface er lidt bøvlet |
| **Postmark** | $15 / 10.000 mails | Meget fokus på deliverability og transactional | Markant dyrere end SES |
| **Mailgun** | $15 / 10.000 mails | Modent API, SMTP, logs/webhooks | Dyrere ved volumen |
| **Resend** | $20 / 50.000 mails | Meget developer-friendly og nemt API | Mindre moden end SES/Mailgun |

Til **dit konkrete tilfælde** ville jeg rangere dem:

1. **SES** – hvis målet er billig, stabil backend til listmonk.
2. **Resend** – hvis du vil have noget betydeligt nemmere og mere lækkert at arbejde med.
3. **Postmark** – hvis transactional mails som trial/signup/password-reset er ekstremt vigtige.
4. **Mailgun** – stadig godt, men jeg synes ikke det giver dig en klar fordel her.

Der er dog en interessant mulighed: **Resend kunne måske gøre setup'et endnu simplere**, fordi de nu både har transactional og marketing-email funktionalitet. De har også webhooks og automations.

Men hvis målet specifikt er **at eje campaign-systemet selv og slippe mest muligt væk fra SaaS-lock-in**, ville jeg stadig tage:

```text
n8n → listmonk → SES
```

Så er SES kun en "dum mail-pipe". Hvis SES en dag irriterer dig, kan du skifte SMTP-provider uden at flytte kontakter, campaigns, templates eller automations.

---

## Bruger — 2026-09-13 15:36 (Europe/Copenhagen)

Det er meget svingende hvor mange e-mails der bliver sendt.  
For vi har folk der joiner free community—> i en drip e-mail campaign med 5x emails over 7 dage. Og så sender jeg 6-7 campaigns om året til hele min liste.

## ChatGPT

Ja — **det mønster gør SES endnu mere oplagt**, især på **shared IPs**.

Du har i praksis to meget forskellige belastninger:

- **Drip-mails:** jævnt men uforudsigeligt flow. Hver ny free-community bruger udløser 5 mails over 7 dage.
- **Campaigns:** kun 6–7 gange om året, men så kommer der pludselig ca. **4.000 mails på én gang**.

AWS anbefaler specifikt shared IPs til **lavere og mindre forudsigelige sending patterns**. Dedicated IP ville være en dårlig idé her, fordi den helst skal have kontinuerlig volumen for at opretholde reputation.

Så:

```text
Free signup
   ↓
n8n
   ↓
5-email drip
   ↓
SES shared IP

+

listmonk
   ↓
Campaign til ~4.000
   ↓
SES shared IP
```

Og økonomien er næsten ligegyldig på dit volumen. SES à-la-carte koster aktuelt **$0.10 pr. 1.000 mails**.

Så én campaign til 4.000:

**$0.40**

7 campaigns om året:

**$2.80/år**

Og eksempelvis 1.000 nye community-medlemmer × 5 drip-mails:

**5.000 mails = $0.50**

Selv hvis du sendte **100.000 mails om året**, er selve SES-delen kun omkring **$10**.

Det vigtigste i dit tilfælde er derfor ikke prisen, men at du undgår at skulle vedligeholde din egen IP-reputation. SES' shared pool kan håndtere, at du sender relativt lidt i en periode og derefter pludselig sender 4.000 mails.

Jeg ville derfor **ikke vælge Postal/self-hosted SMTP til dit sending pattern**. Jeg ville self-hoste listmonk + n8n og bruge SES som transportlaget.

---

## Bruger — 2026-09-13 15:38 (Europe/Copenhagen)

Hvad vil prisen være med resend?  
Mest fordi resend er meget billig at arbejde med

## ChatGPT

Hvis du vil bruge **Resend til både campaigns og drip-emails**, så vil prisen hos dig sandsynligvis være omkring **$40/md.**

Grunden er din liste på ~4.000 kontakter. Resends marketing-plan koster aktuelt:

| Kontakter | Pris |
|---:|---:|
| 1.000 | $0/md |
| 5.000 | **$40/md** |
| 10.000 | $80/md |

På marketing-planen kan du sende **ubegrænsede broadcasts** til de kontakter, så dine 6–7 store campaigns om året er ikke et problem.

Det interessante er, at Resend nu også understøtter **Automations** specifikt til ting som welcome sequences og drip campaigns.

Så du kunne potentielt droppe både GHL **og listmonk/n8n til selve email-flowet** og gøre:

```text
Website / app
      ↓
    Resend
      ↓
Contacts + Audiences
      ↓
 ┌───────────────┐
 │               │
Automations    Broadcasts
 │               │
5-email drip   6-7x campaigns/år
```

Det ville koste cirka **$480/år** ved 5.000-contact tier.

Hvis du derimod bruger **listmonk + Resend kun som SMTP/API**, kan du nøjes med Resends transactional-plan. Pro er **$20/md for 50.000 mails/md**, med $0.90/1.000 i overage.

Så valget er groft sagt:

**$20/md:**  
`n8n + listmonk → Resend`  
Mere self-hosting og kontrol.

**$40/md:**  
`Din app → Resend Automations + Broadcasts`  
Markant simplere setup.

Med det du beskriver, ville jeg faktisk seriøst overveje **$40 Resend-løsningen**. Hvis årsagen til at forlade GHL primært er pris/kompleksitet og ikke et krav om self-hosting, er der måske ingen grund til at drive listmonk + n8n bare for email.

---

## Bruger — 2026-09-13 15:41 (Europe/Copenhagen)

Arh… $480 med resend vs $3.3 med SES…  
Jeg har allerede n8n (og mange andre ting) self hostet på både VPS og egen server

## ChatGPT

Ja, med det setup ville jeg klart vælge **SES + det du allerede self-hoster**.

En vigtig rettelse fra før: SES har ændret prisstruktur i juli 2026, men **à-la-carte er stadig $0.10 pr. 1.000 udgående mails uden minimumsbetaling**.

Så dit setup kunne være:

```text
Din app
   ↓ webhook
  n8n
   ↓
listmonk
   ↓ SMTP
Amazon SES
```

**n8n** styrer flows/logik:
- free community signup
- vent 1 dag
- send mail #2
- check status/tag
- send mail #3 osv.

**listmonk** styrer:
- de ~4.000 kontakter
- segments/tags
- templates
- unsubscribe
- dine 6–7 broadcasts om året
- campaign stats

listmonk kan både sende campaigns og transactional mails via API, så n8n passer rigtig fint ovenpå.

Og **SES shared IP** passer faktisk næsten perfekt til dit ujævne sending pattern. AWS anbefaler shared IPs, når volumen er lav eller uforudsigelig, frem for en dedicated IP der skal holdes varm.

Hvis du fx har:

```text
7 campaigns × 4.000 = 28.000 mails/år
+
2.000 nye free users × 5 mails = 10.000 mails/år

Total = 38.000 mails
```

så er SES-udgiften cirka:

**38 × $0.10 = $3.80/år**

Selv **100.000 mails/år = ca. $10**.

Så når du allerede har VPS, server og n8n, ville jeg ikke betale $40/md for Resend bare for et pænere UI. **listmonk + n8n + SES** er sandsynligvis det mest rationelle setup her.

---

## Bruger — 2026-09-13 15:42 (Europe/Copenhagen)

Hvordan sørger Amazon SES for at man ikke lander på en “dårlig” shared ip?

## ChatGPT

Det kan Amazon ikke garantere i betydningen “du får altid en perfekt IP”. Med **shared IPs** ved du faktisk ikke, hvilken IP din mail går ud fra, og den kan ændre sig.

Det AWS gør er, at de **styrer hele shared poolens reputation** og fordeler trafikken for at holde IP’erne så sunde som muligt. AWS beskriver selv, at de “carefully manage” outbound traffic for at maksimere shared-IP reputation.

De beskytter poolen bl.a. ved at have en **global suppression list** og ved aktivt at begrænse dårlig sending-adfærd, fordi én dårlig sender ellers kunne skade alle andre kunder på samme IP-pool.

Så modellen er mere:

```text
Du sender mail
   ↓
SES vurderer/fordeler trafik
   ↓
En IP fra AWS' shared pool
   ↓
Gmail / Outlook
```

ikke:

```text
Din konto → fast shared IP #123
```

Det betyder også, at hvis en enkelt IP får problemer, er AWS i en langt bedre position til at flytte trafik rundt og håndtere reputation, end du ville være med én selvhostet IP.

Der er dog en vigtig detalje: **IP reputation er kun én del af deliverability**. Gmail og Outlook ser også meget på dit eget domæne, DKIM/SPF/DMARC, bounce-rate, complaints, engagement og historik. Så en god SES shared IP kan ikke redde en dårlig liste.

Til dit volumen ville jeg derfor stadig vælge shared SES frem for dedicated. AWS siger direkte, at shared IPs er bedst til lav volumen, og at dedikerede IP’er giver mere mening ved større, regelmæssige sending volumes.

Hvis du vil, kan jeg også forklare **hvordan jeg ville sætte SES op, så dine marketing-mails ikke risikerer at skade dine transactional mails**.

---

## Bruger — 2026-09-13 15:51 (Europe/Copenhagen)

Og hvorfor ikke selfhoste det hele?  
Enten kører direkte fra min Hjemmeserver, eller igennem CloudFlare?

## ChatGPT

Der er en vigtig skelnen her: **du kan godt self-hoste hele mailstacken**, men Cloudflare kan ikke bare “skjule” din hjemmeservers SMTP-IP og dermed løse reputation-problemet. Cloudflare har dog nu en separat outbound Email Sending-service, som er noget andet.

Du **kan** self-hoste det hele. Problemet er ikke softwaren; det er **outbound IP reputation**.

### Direkte fra din hjemmeserver

Teknisk kunne du køre:

```text
n8n → listmonk → Postal/Postfix → Gmail/Outlook
```

Men din hjemme-IP bliver selve afsender-IP'en. Det giver nogle potentielle problemer:

- ISP'er blokerer ofte outbound port 25.
- Du skal kunne sætte korrekt **PTR / reverse DNS**.
- Residential IP ranges bliver generelt behandlet mere mistænkeligt af mailservere.
- Hvis din IP skifter, starter reputation i praksis forfra.
- Din volumen er meget ujævn, så IP'en får svært ved at bygge stabil reputation.
- Du skal selv håndtere queues, retries, bounces, complaints, throttling og blocklists.

Det sidste punkt er især relevant for dig. AWS beskriver selv, at IP reputation i høj grad bygger på **historisk og konsistent sending volume**, og anbefaler shared IP til lave/uregelmæssige volumener som dine.

Så:

```text
Hjemmeserver
    ↓
egen IP  ← det er denne Gmail vurderer
    ↓
Gmail
```

### Cloudflare foran hjemmeserveren?

Her er den vigtige detalje:

**Den normale orange Cloudflare proxy hjælper ikke på SMTP.**

Cloudflare proxyer som standard ikke SMTP på port 25. Mail-DNS records skal normalt være `DNS only`.

Cloudflare Spectrum kan håndtere SMTP/TCP, men Cloudflare beskriver Spectrum som en proxy/load balancer foran din SMTP-server — **ikke som en intermediary mailserver/MTA**. Det er altså ikke bare en gratis måde at få Cloudflares gode IP reputation på dine self-hostede mails.

### Men Cloudflare har faktisk fået en email-sender

Det interessante er, at Cloudflare i 2026 lancerede **Email Sending**.

Du kan gøre:

```text
listmonk / n8n
       ↓ SMTP/API
Cloudflare Email Service
       ↓
Gmail / Outlook
```

Cloudflare håndterer så bl.a.:

- IP reputation
- SPF
- DKIM
- bounce handling
- suppression lists
- soft-bounce retries

Og prisen er faktisk rimelig:

**3.000 mails/md inkluderet**, derefter **$0.35 / 1.000 mails**, men det kræver Workers Paid.

Der er bare én ret stor dealbreaker for dig lige nu:

> Cloudflare siger eksplicit, at Email Service kun er beregnet til **transactional emails**, og at marketing/bulk emails endnu ikke understøttes.

Så jeg ville **ikke** sende dine 4.000-personers campaigns gennem Cloudflare Email Sending på nuværende tidspunkt.

### Hvad hvis du virkelig vil self-hoste?

Så ville jeg ikke bruge hjemmeserveren. Jeg ville gøre:

```text
                  ┌─ n8n
                  │
Contacts → listmonk
                  │
                  └─ campaigns
                       ↓
                     Postal
                       ↓
             VPS med statisk IPv4
                       ↓
                  Gmail/Outlook
```

VPS'en skal have:

```text
Static IPv4
Port 25 outbound
PTR/rDNS → mail.example.com
SPF
DKIM
DMARC
TLS
FQDN/HELO korrekt
```

**Det kan fungere rigtig godt.**

Men ironien i dit tilfælde er økonomien:

```text
Self-hosted SMTP:
€0-ish ekstra
+ din tid
+ reputation management
+ risiko for blocklists

SES:
~$3-10 OM ÅRET
+ AWS klarer IP-pool/reputation
```

Derfor ville jeg stadig vælge:

**`n8n + listmonk self-hosted → SES shared IP`**

Ikke fordi self-hosting er umuligt, men fordi **SES praktisk talt er gratis ved dit volumen**, og det outsourcer præcis den ene del af email, som er irriterende at self-hoste: **IP reputation og aflevering til Gmail/Outlook**.

Hvis dit mål også er at lære det eller være 100% uafhængig, er **Postal på en VPS med dedikeret IPv4** derimod et interessant projekt. Hjemmeserveren ville jeg undgå til selve outbound SMTP.

---

## Bruger — 2026-09-29 09:39 (Europe/Copenhagen)

kan du eksportere denne chat så jeg kan downloade den som en stor .md fil
