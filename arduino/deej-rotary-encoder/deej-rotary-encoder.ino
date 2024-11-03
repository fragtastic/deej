// Pin definitions for ATmega32U4
const int encoderPinA = 3;  // Encoder A pin (INT1)
const int encoderPinB = 2;  // Encoder B pin (INT0)
const int buttonPin = 7;    // Button pin (INT6)

// Button debouncing
const unsigned long debounceDelay = 50; // Debounce delay in milliseconds
volatile bool buttonState = HIGH;       // Current button state
volatile bool lastButtonState = HIGH;   // Previous button state
volatile unsigned long lastDebounceTime = 0;  // Time the button state last changed

// Rotary encoder tracking
volatile int encoderPosition = 0;  // To track the overall position
volatile int lastState = 0;        // To store the last state of the encoder
volatile bool direction = false;   // false = left, true = right
volatile bool newDirectionAvailable = false;  // To signal if a new rotation has occurred

void setup() {
  // Set up pins
  pinMode(encoderPinA, INPUT);
  pinMode(encoderPinB, INPUT);
  pinMode(buttonPin, INPUT_PULLUP);  // Use internal pull-up resistor for button

  // Initialize serial communication at high speed
  Serial.begin(115200);

  // Attach interrupts for encoder and button
  attachInterrupt(digitalPinToInterrupt(encoderPinA), handleEncoder, CHANGE);
  attachInterrupt(digitalPinToInterrupt(encoderPinB), handleEncoder, CHANGE);
  attachInterrupt(digitalPinToInterrupt(buttonPin), handleButton, CHANGE);
}

void loop() {
  // Handle button press and release with debouncing
  if ((millis() - lastDebounceTime) > debounceDelay) {
    if (buttonState == LOW && lastButtonState == HIGH) {
      Serial.print("d\n");  // Button pressed (down)
      lastButtonState = buttonState;
    } 
    else if (buttonState == HIGH && lastButtonState == LOW) {
      Serial.print("u\n");  // Button released (up)
      lastButtonState = buttonState;
    }
  }

  // Continuously send direction if a new movement is detected
  if (newDirectionAvailable) {
    if (direction) {
      Serial.print("r\n");  // Turned right
    } else {
      Serial.print("l\n");  // Turned left
    }
    newDirectionAvailable = false;  // Reset after sending the message
  }
}

// Interrupt service routine for rotary encoder
void handleEncoder() {
  // Read the current state of encoder pins
  int currentStateA = digitalRead(encoderPinA);
  int currentStateB = digitalRead(encoderPinB);
  
  // Combine the two states to get the full state of the encoder
  int currentState = (currentStateA << 1) | currentStateB;

  // Determine the direction based on the state transition
  if (lastState == 0b00 && currentState == 0b01) { 
    direction = false;  // Moving left
  } else if (lastState == 0b01 && currentState == 0b11) {
    direction = false;  // Moving left
  } else if (lastState == 0b11 && currentState == 0b10) {
    direction = false;  // Moving left
  } else if (lastState == 0b10 && currentState == 0b00) {
    direction = false;  // Moving left
    encoderPosition--;  // Full step completed
    newDirectionAvailable = true;  // Report movement
  } else if (lastState == 0b00 && currentState == 0b10) { 
    direction = true;  // Moving right
  } else if (lastState == 0b10 && currentState == 0b11) {
    direction = true;  // Moving right
  } else if (lastState == 0b11 && currentState == 0b01) {
    direction = true;  // Moving right
  } else if (lastState == 0b01 && currentState == 0b00) {
    direction = true;  // Moving right
    encoderPosition++;  // Full step completed
    newDirectionAvailable = true;  // Report movement
  }

  // Store the current state as the last state for the next interrupt
  lastState = currentState;
}

// Interrupt service routine for button with debouncing
void handleButton() {
  unsigned long currentTime = millis();

  if ((currentTime - lastDebounceTime) > debounceDelay) {
    buttonState = digitalRead(buttonPin);
    lastDebounceTime = currentTime;
  }
}
