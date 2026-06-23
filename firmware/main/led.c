/* Status LED GPIO21. Verify polarity (active-high) on the first device test;
 * if it lights inverted -> flip LED_ON_LEVEL. */
#include "led.h"
#include "driver/gpio.h"
#include "esp_timer.h"

#define LED_GPIO        21
#define LED_ON_LEVEL    1            /* 1 = active-high; set to 0 if needed */
#define BLINK_PERIOD_US 150000       /* ~3.3 Hz transfer blink */

static esp_timer_handle_t s_blink;
static bool s_blink_on;

static inline void drive(int on)
{
    gpio_set_level(LED_GPIO, on ? LED_ON_LEVEL : !LED_ON_LEVEL);
}

void led_init(void)
{
    gpio_config_t c = {
        .pin_bit_mask = 1ULL << LED_GPIO,
        .mode = GPIO_MODE_OUTPUT,
        .pull_up_en = GPIO_PULLUP_DISABLE,
        .pull_down_en = GPIO_PULLDOWN_DISABLE,
        .intr_type = GPIO_INTR_DISABLE,
    };
    gpio_config(&c);
    drive(0);
}

static void blink_cb(void *arg)
{
    s_blink_on = !s_blink_on;
    drive(s_blink_on);
}

void led_blink_start(void)
{
    if (!s_blink) {
        const esp_timer_create_args_t a = { .callback = blink_cb, .name = "led_blink" };
        esp_timer_create(&a, &s_blink);
    }
    s_blink_on = true;
    drive(1);
    esp_timer_start_periodic(s_blink, BLINK_PERIOD_US);   /* idempotent enough for our path */
}

void led_blink_stop(void)
{
    if (s_blink) esp_timer_stop(s_blink);   /* INVALID_STATE if not running -> ignore */
}

void led_on(void)  { led_blink_stop(); drive(1); }
void led_off(void) { led_blink_stop(); drive(0); }
