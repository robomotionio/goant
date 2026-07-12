function check(a,b){if(a!==b)throw a+" !== "+b;} var ok=false;try{(Infinity).toExponential(2);}catch(e){ok=true;}check((Infinity).toExponential(2),'Infinity');console.log('PASS');
