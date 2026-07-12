function check(a,b){if(a!==b)throw a+" !== "+b;} check((3.14159).toFixed(2),'3.14');check((0).toFixed(2),'0.00');check((1.5).toFixed(0),'2');console.log('PASS');
